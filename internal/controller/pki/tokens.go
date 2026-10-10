package pki

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

const tokenBytes = 32

// newToken returns 256 random bits as base64url.
func newToken() (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("pki: generating token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// hashToken is what the database stores in place of the token.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
