package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxDrift is how far a request timestamp may differ from the server clock.
const maxDrift = 5 * time.Minute

// TokenTTL is how long a login token remains valid.
const TokenTTL = 24 * time.Hour

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
