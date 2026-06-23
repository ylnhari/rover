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
