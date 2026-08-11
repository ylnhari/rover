package auth_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/ylnhari/rover/internal/auth"
)

func TestSignAndVerify(t *testing.T) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := auth.Sign("mysecret", ts, `{"command":"echo hi"}`)

	if err := auth.Verify("mysecret", ts, `{"command":"echo hi"}`, sig); err != nil {
		t.Fatalf("expected valid signature: %v", err)
	}
}

func TestVerifyWrongSecret(t *testing.T) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := auth.Sign("secret-a", ts, "body")

	if err := auth.Verify("secret-b", ts, "body", sig); err == nil {
		t.Fatal("expected signature mismatch error")
	}
}

func TestVerifyStaleTimestamp(t *testing.T) {
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	sig := auth.Sign("secret", stale, "body")

	if err := auth.Verify("secret", stale, "body", sig); err == nil {
		t.Fatal("expected drift error for 10-minute-old timestamp")
	}
}

func TestVerifyTamperedBody(t *testing.T) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := auth.Sign("secret", ts, "original-body")

	if err := auth.Verify("secret", ts, "tampered-body", sig); err == nil {
		t.Fatal("expected signature mismatch for tampered body")
	}
}

func TestIssueAndVerifyToken(t *testing.T) {
	token, err := auth.IssueToken("mysecret")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if err := auth.VerifyToken("mysecret", token); err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
}

func TestVerifyTokenWrongSecret(t *testing.T) {
	token, err := auth.IssueToken("secret-a")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if err := auth.VerifyToken("secret-b", token); err == nil {
		t.Fatal("expected signature mismatch")
	}
}

func TestVerifyTokenInvalidFormat(t *testing.T) {
	if err := auth.VerifyToken("secret", "notavalidtoken"); err == nil {
		t.Fatal("expected format error")
	}
}

func TestVerifyTokenTampered(t *testing.T) {
	token, _ := auth.IssueToken("secret")
	// Flip the last character of the signature
	tampered := token[:len(token)-1] + "x"
	if err := auth.VerifyToken("secret", tampered); err == nil {
		t.Fatal("expected signature mismatch for tampered token")
	}
}

func TestProxyTokenIsPurposeSeparated(t *testing.T) {
	control, err := auth.IssueToken("test-secret")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	proxy, err := auth.IssueProxyToken("test-secret")
	if err != nil {
		t.Fatalf("IssueProxyToken: %v", err)
	}

	if control == proxy {
		t.Fatal("control and proxy credentials must differ")
	}
	if err := auth.VerifyToken("test-secret", proxy); err == nil {
		t.Fatal("proxy credential must not authorize control APIs")
	}
	if err := auth.VerifyProxyToken("test-secret", control); err == nil {
		t.Fatal("control credential must not authorize a project proxy")
	}
	if err := auth.VerifyProxyToken("test-secret", proxy); err != nil {
		t.Fatalf("VerifyProxyToken: %v", err)
	}
}

func TestProxyRequestProofIsFreshAndRequestBound(t *testing.T) {
	const masterSecret = "test-only-master-secret"
	key := auth.DeriveProxyRequestKey(masterSecret, "mycard-benefits")
	otherKey := auth.DeriveProxyRequestKey(masterSecret, "another-project")
	if key == masterSecret || key == otherKey {
		t.Fatal("project verifier value was not purpose- and project-separated")
	}
	verifier := auth.NewProxyRequestVerifier()
	proof1 := auth.IssueProxyRequestProof(key, "127.0.0.1:8777", "POST", "/cards?view=full")
	proof2 := auth.IssueProxyRequestProof(key, "127.0.0.1:8777", "POST", "/cards?view=full")

	if proof1 == proof2 {
		t.Fatal("two requests received the same proxy proof")
	}
	if err := verifier.Verify(key, proof1, "127.0.0.1:8777", "POST", "/cards?view=full"); err != nil {
		t.Fatalf("ProxyRequestVerifier.Verify: %v", err)
	}
	if err := verifier.Verify(key, proof1, "127.0.0.1:8777", "POST", "/cards?view=full"); err == nil {
		t.Fatal("identical proxy proof replay was accepted")
	}
	if err := verifier.Verify(key, proof2, "127.0.0.1:8777", "GET", "/cards?view=full"); err == nil {
		t.Fatal("proxy proof must be bound to the method")
	}
	if err := verifier.Verify(key, proof2, "127.0.0.1:8777", "POST", "/cards?view=masked"); err == nil {
		t.Fatal("proxy proof must be bound to the request path")
	}
	if err := verifier.Verify(key, proof2, "127.0.0.1:9999", "POST", "/cards?view=full"); err == nil {
		t.Fatal("proxy proof must be bound to the target backend")
	}
	if err := verifier.Verify(masterSecret, proof2, "127.0.0.1:8777", "POST", "/cards?view=full"); err == nil {
		t.Fatal("master secret was accepted in place of the project verifier value")
	}
	if err := verifier.Verify(otherKey, proof2, "127.0.0.1:8777", "POST", "/cards?view=full"); err == nil {
		t.Fatal("another project's verifier value was accepted")
	}
	if err := verifier.Verify(key, proof2, "127.0.0.1:8777", "POST", "/cards?view=full"); err != nil {
		t.Fatalf("second fresh proxy proof was rejected: %v", err)
	}
}

func TestProxyRequestVerifierRejectsConcurrentReplay(t *testing.T) {
	key := auth.DeriveProxyRequestKey("test-only-master-secret", "concurrent-project")
	proof := auth.IssueProxyRequestProof(key, "127.0.0.1:8777", "GET", "/cards")
	verifier := auth.NewProxyRequestVerifier()
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- verifier.Verify(key, proof, "127.0.0.1:8777", "GET", "/cards")
		}()
	}
	close(start)
	accepted := 0
	for range 2 {
		if err := <-results; err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("concurrent replay accepted %d times; want exactly 1", accepted)
	}
}
