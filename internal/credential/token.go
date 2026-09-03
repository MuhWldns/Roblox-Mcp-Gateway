// Package credential provides opaque, keyed credentials.
package credential

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

var errInvalidBytes = errors.New("credential: bytes must be positive")

// Generate returns an opaque URL-safe credential and its keyed digest. The
// plaintext is never retained by this package.
func Generate(prefix string, bytes int, pepper []byte) (string, [32]byte, error) {
	if bytes <= 0 {
		return "", [32]byte{}, errInvalidBytes
	}

	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", [32]byte{}, err
	}

	plain := prefix + base64.RawURLEncoding.EncodeToString(raw)
	return plain, Digest(plain, pepper), nil
}

// Digest computes the HMAC-SHA-256 digest of plain under pepper.
func Digest(plain string, pepper []byte) [32]byte {
	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write([]byte(plain))

	var digest [32]byte
	copy(digest[:], mac.Sum(nil))
	return digest
}

// EqualDigest compares digests in constant time.
func EqualDigest(a, b [32]byte) bool {
	return hmac.Equal(a[:], b[:])
}

// Equal is an alias for EqualDigest for callers comparing keyed digests.
func Equal(a, b [32]byte) bool {
	return EqualDigest(a, b)
}
