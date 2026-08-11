package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxDrift is how far a request timestamp may differ from the server clock.
const maxDrift = 5 * time.Minute

// TokenTTL is how long a login token remains valid.
const TokenTTL = 24 * time.Hour

const (
	proxyTokenContext       = "rover-proxy-cookie-v1"
	proxyRequestKeyContext  = "rover-proxy-verifier-key-v1"
	proxyRequestContext     = "rover-proxy-request-v1"
	proxyRequestMaxClockAge = 30 * time.Second
	maxAcceptedProxyProofs  = 65536
)

var lastProxyRequestTimestamp atomic.Int64

// Sign returns HMAC-SHA256(secret, timestamp+":"+body) as a lowercase hex string.
func Sign(secret, timestamp, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s:%s", timestamp, body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks that signature matches and that the timestamp is within maxDrift
// of the current time. Returns nil on success, a descriptive error otherwise.
func Verify(secret, timestamp, body, signature string) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp format")
	}
	age := time.Since(time.Unix(ts, 0))
	if age > maxDrift || age < -maxDrift {
		return fmt.Errorf("timestamp drift too large: %v", age)
	}
	expected := Sign(secret, timestamp, body)
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// IssueToken creates a stateless, HMAC-signed token that expires after TokenTTL.
// Format: <unix_timestamp>.<random_hex>.<hmac_hex>
func IssueToken(secret string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonceHex := hex.EncodeToString(nonce)
	sig := Sign(secret, ts, nonceHex)
	return ts + "." + nonceHex + "." + sig, nil
}

// VerifyToken validates a token issued by IssueToken and checks it has not expired.
func VerifyToken(secret, token string) error {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return fmt.Errorf("invalid token format")
	}
	ts, nonceHex, sig := parts[0], parts[1], parts[2]

	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid token timestamp")
	}
	age := time.Since(time.Unix(tsInt, 0))
	if age > TokenTTL {
		return fmt.Errorf("token expired")
	}
	if age < -time.Minute {
		return fmt.Errorf("token issued in the future")
	}

	expected := Sign(secret, ts, nonceHex)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("invalid token signature")
	}
	return nil
}

// IssueProxyToken creates a 24-hour browser credential scoped to project
// proxies. Its format and HMAC context are deliberately distinct from control
// API tokens, so neither verifier can accept the other credential type.
func IssueProxyToken(secret string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonceHex := hex.EncodeToString(nonce)
	sig := scopedSign(secret, proxyTokenContext, ts, nonceHex)
	return "p1." + ts + "." + nonceHex + "." + sig, nil
}

// VerifyProxyToken validates only credentials minted by IssueProxyToken.
func VerifyProxyToken(secret, token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != "p1" {
		return fmt.Errorf("invalid proxy token format")
	}
	ts, nonceHex, sig := parts[1], parts[2], parts[3]
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid proxy token timestamp")
	}
	age := time.Since(time.Unix(tsInt, 0))
	if age > TokenTTL {
		return fmt.Errorf("proxy token expired")
	}
	if age < -time.Minute {
		return fmt.Errorf("proxy token issued in the future")
	}
	expected := scopedSign(secret, proxyTokenContext, ts, nonceHex)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("invalid proxy token signature")
	}
	return nil
}

// DeriveProxyRequestKey returns a project-scoped verifier value. A child that
// receives it can validate (or forge) proofs only for that project; it cannot
// recover the rover master secret or derive another project's value.
func DeriveProxyRequestKey(secret, projectID string) string {
	return scopedSign(secret, proxyRequestKeyContext, projectID)
}

// IssueProxyRequestProof returns the value for X-Rover-Proxy. The proof is
// bound to one backend audience, method, and exact request URI and carries a
// unique Unix-nanosecond timestamp. key must be a project-scoped value from
// DeriveProxyRequestKey, not the rover master secret.
func IssueProxyRequestProof(key, audience, method, requestURI string) string {
	ts := strconv.FormatInt(nextProxyRequestTimestamp(), 10)
	sig := scopedSign(key, proxyRequestContext, ts, audience, method, requestURI)
	return "v1." + ts + "." + sig
}

type acceptedProxyProof struct {
	proof   string
	expires int64
}

// ProxyRequestVerifier validates X-Rover-Proxy proofs and remembers accepted
// proofs for their freshness window so an identical proof is one-use.
type ProxyRequestVerifier struct {
	mu          sync.Mutex
	accepted    map[string]int64
	expiryQueue []acceptedProxyProof
	expiryHead  int
}

func NewProxyRequestVerifier() *ProxyRequestVerifier {
	return &ProxyRequestVerifier{accepted: make(map[string]int64)}
}

// Verify validates signature, backend/request binding, freshness, and replay.
// The cache is time-bounded and fails closed at its hard size limit.
func (v *ProxyRequestVerifier) Verify(key, proof, audience, method, requestURI string) error {
	nanos, err := verifyProxyRequestProof(key, proof, audience, method, requestURI)
	if err != nil {
		return err
	}

	now := time.Now().UnixNano()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.accepted == nil {
		v.accepted = make(map[string]int64)
	}
	v.pruneExpired(now)
	if _, exists := v.accepted[proof]; exists {
		return fmt.Errorf("proxy request proof replayed")
	}
	if len(v.accepted) >= maxAcceptedProxyProofs {
		return fmt.Errorf("proxy request replay cache full")
	}
	expires := nanos + proxyRequestMaxClockAge.Nanoseconds()
	v.accepted[proof] = expires
	v.expiryQueue = append(v.expiryQueue, acceptedProxyProof{proof: proof, expires: expires})
	return nil
}

func (v *ProxyRequestVerifier) pruneExpired(now int64) {
	for v.expiryHead < len(v.expiryQueue) {
		entry := v.expiryQueue[v.expiryHead]
		if entry.expires >= now {
			break
		}
		if v.accepted[entry.proof] == entry.expires {
			delete(v.accepted, entry.proof)
		}
		v.expiryHead++
	}
	// Periodically release references while keeping cleanup amortized O(1).
	if v.expiryHead >= 1024 && v.expiryHead*2 >= len(v.expiryQueue) {
		v.expiryQueue = append([]acceptedProxyProof(nil), v.expiryQueue[v.expiryHead:]...)
		v.expiryHead = 0
	}
}

func verifyProxyRequestProof(key, proof, audience, method, requestURI string) (int64, error) {
	parts := strings.Split(proof, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return 0, fmt.Errorf("invalid proxy request proof format")
	}
	ts, sig := parts[1], parts[2]
	nanos, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid proxy request timestamp")
	}
	age := time.Since(time.Unix(0, nanos))
	if age > proxyRequestMaxClockAge || age < -proxyRequestMaxClockAge {
		return 0, fmt.Errorf("proxy request timestamp drift too large: %v", age)
	}
	expected := scopedSign(key, proxyRequestContext, ts, audience, method, requestURI)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return 0, fmt.Errorf("invalid proxy request signature")
	}
	return nanos, nil
}

func scopedSign(secret, context string, fields ...string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(context))
	for _, field := range fields {
		mac.Write([]byte{0})
		mac.Write([]byte(field))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func nextProxyRequestTimestamp() int64 {
	now := time.Now().UnixNano()
	for {
		previous := lastProxyRequestTimestamp.Load()
		if now <= previous {
			now = previous + 1
		}
		if lastProxyRequestTimestamp.CompareAndSwap(previous, now) {
			return now
		}
	}
}
