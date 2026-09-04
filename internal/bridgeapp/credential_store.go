package bridgeapp

import (
	"errors"
	"strings"
)

// CredentialStore persists the Bridge device credential using OS-level,
// user-bound protection. Plaintext must never reach the disk: the stored
// bytes must be unusable when copied to another machine or decrypted by
// another user.
type CredentialStore interface {
	Load() ([]byte, error)
	Save([]byte) error
	Delete() error
}

var (
	// ErrUnsupportedSecureStore indicates the platform provides no supported
	// user-bound secret store. Callers must not fall back to plaintext.
	ErrUnsupportedSecureStore = errors.New("bridgeapp: no supported secure credential store on this platform")
	// ErrInvalidCredentialPath indicates a blank credential file path.
	ErrInvalidCredentialPath = errors.New("bridgeapp: credential path is required")
)

// NewFileCredentialStore returns the platform-backed credential store for the
// given file path. On Windows the file holds a DPAPI blob bound to the
// current user; other platforms receive an unusable store that reports
// ErrUnsupportedSecureStore from every operation.
func NewFileCredentialStore(path string) (CredentialStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ErrInvalidCredentialPath
	}
	return newPlatformCredentialStore(path)
}
