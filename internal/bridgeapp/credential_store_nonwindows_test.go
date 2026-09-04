//go:build !windows

package bridgeapp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCredentialStoreUnsupportedOffWindows proves the secure store refuses to
// operate outside Windows instead of falling back to plaintext storage: the
// constructor reports ErrUnsupportedSecureStore and every method keeps
// refusing without writing a file.
func TestCredentialStoreUnsupportedOffWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.credential")
	store, err := NewFileCredentialStore(path)
	if !errors.Is(err, ErrUnsupportedSecureStore) {
		t.Fatalf("constructor error = %v, want ErrUnsupportedSecureStore", err)
	}
	if store == nil {
		t.Fatal("constructor returned a nil store; want an unusable store carrying the error")
	}

	if err := store.Save([]byte("secret")); !errors.Is(err, ErrUnsupportedSecureStore) {
		t.Fatalf("Save error = %v, want ErrUnsupportedSecureStore", err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrUnsupportedSecureStore) {
		t.Fatalf("Load error = %v, want ErrUnsupportedSecureStore", err)
	}
	if err := store.Delete(); !errors.Is(err, ErrUnsupportedSecureStore) {
		t.Fatalf("Delete error = %v, want ErrUnsupportedSecureStore", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unsupported store wrote a credential file")
	}
}
