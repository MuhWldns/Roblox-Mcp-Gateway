package bridgeapp

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewFileCredentialStoreRejectsBlankPath(t *testing.T) {
	if _, err := NewFileCredentialStore("  "); !errors.Is(err, ErrInvalidCredentialPath) {
		t.Fatalf("blank path error = %v, want ErrInvalidCredentialPath", err)
	}
}

func TestFileCredentialStoreContract(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("DPAPI-backed store is only supported on Windows")
	}
	path := filepath.Join(t.TempDir(), "bridge.credential")
	store, err := NewFileCredentialStore(path)
	if err != nil {
		t.Fatalf("construct credential store: %v", err)
	}

	secret := []byte("rkd_live_device_credential_sample_value_0123456789")
	if err := store.Save(secret); err != nil {
		t.Fatalf("save: %v", err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw credential file: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("credential file is empty")
	}
	if string(stored) == string(secret) {
		t.Fatal("credential file contains the plaintext credential")
	}
	for _, chunk := range []string{string(secret), "rkd_live_device_credential"} {
		if contains(stored, []byte(chunk)) {
			t.Fatalf("credential file leaks plaintext substring %q", chunk)
		}
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(loaded) != string(secret) {
		t.Fatalf("round-trip mismatch: got %q want %q", loaded, secret)
	}

	rotated := []byte("rkd_rotated_credential_abcdef")
	if err := store.Save(rotated); err != nil {
		t.Fatalf("save rotated: %v", err)
	}
	if loaded, err = store.Load(); err != nil || string(loaded) != string(rotated) {
		t.Fatalf("rotated load = %q, %v", loaded, err)
	}

	if err := store.Delete(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("load after delete unexpectedly succeeded")
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestFileCredentialStoreLoadMissingFileFails(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("DPAPI-backed store is only supported on Windows")
	}
	path := filepath.Join(t.TempDir(), "bridge.credential")
	store, err := NewFileCredentialStore(path)
	if err != nil {
		t.Fatalf("construct credential store: %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("load of a missing credential unexpectedly succeeded")
	}
}

func contains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
