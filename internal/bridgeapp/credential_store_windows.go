//go:build windows

package bridgeapp

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// dpapiStore persists the device credential as a Windows DPAPI blob bound to
// the current user (CRYPTPROTECT_UI_FORBIDDEN, per-user key material). The
// file bytes are meaningless on another machine or for another account.
type dpapiStore struct {
	path string
}

// dataBlob mirrors Win32 DATA_BLOB.
type dataBlob struct {
	size uint32
	data *byte
}

const cryptProtectUIForbidden = 0x1

var (
	crypt32       = syscall.NewLazyDLL("crypt32.dll")
	protectProc   = crypt32.NewProc("CryptProtectData")
	unprotectProc = crypt32.NewProc("CryptUnprotectData")
	localFreeProc = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")
)

func newPlatformCredentialStore(path string) (CredentialStore, error) {
	return &dpapiStore{path: path}, nil
}

// Save encrypts the credential with DPAPI and writes the blob to disk.
func (s *dpapiStore) Save(data []byte) error {
	if len(data) == 0 {
		return errors.New("bridgeapp: credential must not be empty")
	}
	blob, err := protect(data)
	if err != nil {
		return fmt.Errorf("bridgeapp: protect credential: %w", err)
	}
	if err := os.WriteFile(s.path, blob, 0o600); err != nil {
		return fmt.Errorf("bridgeapp: write credential file: %w", err)
	}
	return nil
}

// Load reads the DPAPI blob and decrypts it under the current user.
func (s *dpapiStore) Load() ([]byte, error) {
	blob, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("bridgeapp: read credential file: %w", err)
	}
	data, err := unprotect(blob)
	if err != nil {
		return nil, fmt.Errorf("bridgeapp: unprotect credential: %w", err)
	}
	return data, nil
}

// Delete removes the credential file; deleting a missing file succeeds.
func (s *dpapiStore) Delete() error {
	if err := os.Remove(s.path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("bridgeapp: delete credential file: %w", err)
	}
	return nil
}

func protect(data []byte) ([]byte, error) {
	in := dataBlob{size: uint32(len(data)), data: &data[0]}
	var out dataBlob
	ret, _, callErr := protectProc.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // szDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, callErr
	}
	defer localFreeCall(out)
	return copyOut(out), nil
}

func unprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("empty DPAPI blob")
	}
	in := dataBlob{size: uint32(len(blob)), data: &blob[0]}
	var out dataBlob
	ret, _, callErr := unprotectProc.Call(
		uintptr(unsafe.Pointer(&in)),
		0,
		0,
		0,
		0,
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, callErr
	}
	defer localFreeCall(out)
	return copyOut(out), nil
}

func localFreeCall(blob dataBlob) {
	if blob.data != nil {
		_, _, _ = localFreeProc.Call(uintptr(unsafe.Pointer(blob.data)))
	}
}

// copyOut copies the LocalAlloc-protected buffer before it is freed.
func copyOut(blob dataBlob) []byte {
	if blob.data == nil || blob.size == 0 {
		return nil
	}
	data := make([]byte, blob.size)
	copy(data, unsafe.Slice(blob.data, blob.size))
	return data
}
