//go:build !windows

package bridgeapp

// unsupportedStore refuses every operation: no user-bound secret store is
// supported off Windows, and falling back to plaintext storage is forbidden.
type unsupportedStore struct{}

func newPlatformCredentialStore(string) (CredentialStore, error) {
	return unsupportedStore{}, ErrUnsupportedSecureStore
}

func (unsupportedStore) Save([]byte) error     { return ErrUnsupportedSecureStore }
func (unsupportedStore) Load() ([]byte, error) { return nil, ErrUnsupportedSecureStore }
func (unsupportedStore) Delete() error         { return ErrUnsupportedSecureStore }
