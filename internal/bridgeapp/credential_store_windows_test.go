//go:build windows

package bridgeapp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestDPAPIStorePersistsNoPlaintext proves the on-disk bytes are a DPAPI blob:
// the secret never appears verbatim and the same user can round-trip it.
func TestDPAPIStorePersistsNoPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.credential")
	store, err := NewFileCredentialStore(path)
	if err != nil {
		t.Fatalf("construct credential store: %v", err)
	}

	secret := []byte("rkd_dpapi_secret_check_1234567890abcdef")
	if err := store.Save(secret); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw file: %v", err)
	}
	if bytes.Equal(raw, secret) {
		t.Fatal("credential file stores the plaintext secret verbatim")
	}
	if bytes.Contains(raw, secret) {
		t.Fatal("DPAPI blob contains the plaintext secret")
	}
	if len(raw) < len(secret) {
		t.Fatalf("DPAPI blob unexpectedly short: %d bytes", len(raw))
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("load under the same user: %v", err)
	}
	if !bytes.Equal(loaded, secret) {
		t.Fatalf("round-trip mismatch: got %q", loaded)
	}
}

// TestDPAPIStoreSaveOverwritesPreviousBlob confirms rotation replaces the old
// protected bytes so stale credentials do not linger on disk.
func TestDPAPIStoreSaveOverwritesPreviousBlob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.credential")
	store, err := NewFileCredentialStore(path)
	if err != nil {
		t.Fatalf("construct credential store: %v", err)
	}
	first := []byte("rkd_first_credential_value")
	if err := store.Save(first); err != nil {
		t.Fatalf("save first: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw file: %v", err)
	}
	second := []byte("rkd_second_credential_value")
	if err := store.Save(second); err != nil {
		t.Fatalf("save second: %v", err)
	}
	rotated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rotated file: %v", err)
	}
	if bytes.Equal(raw, rotated) {
		t.Fatal("rotation left the previous DPAPI blob untouched")
	}
	loaded, err := store.Load()
	if err != nil || !bytes.Equal(loaded, second) {
		t.Fatalf("rotated load = %q, %v", loaded, err)
	}
}
