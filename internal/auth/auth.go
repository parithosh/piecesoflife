// Package auth provides cryptographic token generation and hashing utilities.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// GenerateRandomToken creates a cryptographically random token and its SHA-256 hash.
// The raw token is sent to the user (via email/cookie), and the hash is stored in the DB.
func GenerateRandomToken(byteLength int) (raw string, hash string, err error) {
	b := make([]byte, byteLength)

	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generating random bytes: %w", err)
	}

	raw = hex.EncodeToString(b)
	hash = SHA256Hex(raw)

	return raw, hash, nil
}

// SHA256Hex returns the hex-encoded SHA-256 hash of a string.
func SHA256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// GenerateRandomString creates a cryptographically random hex string.
func GenerateRandomString(byteLength int) string {
	b := make([]byte, byteLength)

	//nolint:errcheck // rand.Read on crypto/rand never fails in practice
	rand.Read(b)

	return hex.EncodeToString(b)
}

// GenerateSignedCSRFToken returns a token of the form "<nonce>.<signature>"
// where nonce is 32 hex chars (16 random bytes) and signature is the
// URL-safe base64 of HMAC-SHA256(secret, nonce). The cookie value carries
// both halves; the header echoes the cookie verbatim (double-submit), and
// ValidateSignedCSRFToken additionally checks the signature so an attacker
// who can forge a cookie value (e.g. via a sibling subdomain) still can't
// produce a token that passes server validation.
func GenerateSignedCSRFToken(secret string) string {
	nonce := GenerateRandomString(16)
	return nonce + "." + signCSRFNonce(secret, nonce)
}

// ValidateSignedCSRFToken returns true iff token is a well-formed signed
// CSRF token whose signature matches the given secret. Comparison is
// constant-time.
func ValidateSignedCSRFToken(secret, token string) bool {
	idx := strings.IndexByte(token, '.')
	if idx <= 0 || idx == len(token)-1 {
		return false
	}

	nonce := token[:idx]
	gotSig := token[idx+1:]
	wantSig := signCSRFNonce(secret, nonce)

	return subtle.ConstantTimeCompare([]byte(gotSig), []byte(wantSig)) == 1
}

func signCSRFNonce(secret, nonce string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(nonce))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
