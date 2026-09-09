//go:build !windows

package bridgeapp

import "errors"

// MachineFingerprintHash reports that the hardware-bound enrollment identity
// is unavailable. Production Bridge releases target Windows.
func MachineFingerprintHash() (string, error) {
	return "", errors.New("bridgeapp: hardware fingerprint not supported on this platform")
}
