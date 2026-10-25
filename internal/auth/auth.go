// Package auth checks the bearer keys that protect Forge's three HTTP APIs
// (submission, worker and operator). Each API has its own key, so a leaked
// worker key cannot submit jobs and a submitter's key cannot read operator
// metrics.
//
// These are shared static secrets, not user accounts: they separate roles but
// do not identify a person or a tenant, and they travel in clear text unless
// the connection is protected another way (a private network, or a proxy that adds TLS).
package auth

import (
	"crypto/sha256"
	"fmt"
)

// MinKeyLength is the shortest key accepted, long enough to resist guessing.
const MinKeyLength = 32

// Key is a bearer key kept only as its SHA-256 digest; the plain text is not
// retained.
type Key struct{ sum [sha256.Size]byte }

// NewKey hashes the key. name is used in the error message ("worker API key").
func NewKey(name, plain string) (Key, error) {
	if len(plain) < MinKeyLength {
		return Key{}, fmt.Errorf("%s must be at least %d characters", name, MinKeyLength)
	}
	return Key{sum: sha256.Sum256([]byte(plain))}, nil
}
