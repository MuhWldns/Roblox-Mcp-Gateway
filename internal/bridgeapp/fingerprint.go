package bridgeapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const fingerprintHMACKey = "robloxbridge-device-fp-v1"

// deriveMachineFingerprintHash produces the stable, versioned hardware digest
// sent during enrollment. Labels preserve source identity when one optional
// source is unavailable; values are normalized because registry and firmware
// casing is not hardware identity.
func deriveMachineFingerprintHash(machineGUID, productUUID, volumeSerial string) (string, error) {
	values := []struct {
		label string
		value string
	}{
		{label: "machine_guid", value: machineGUID},
		{label: "product_uuid", value: productUUID},
		{label: "volume_serial", value: volumeSerial},
	}
	parts := make([]string, 0, len(values))
	for _, source := range values {
		value := strings.ToLower(strings.TrimSpace(source.value))
		if value == "" {
			continue
		}
		parts = append(parts, source.label+"="+value)
	}
	if len(parts) < 2 {
		return "", fmt.Errorf("bridgeapp: insufficient fingerprint sources (%d/3)", len(parts))
	}
	mac := hmac.New(sha256.New, []byte(fingerprintHMACKey))
	_, _ = mac.Write([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(mac.Sum(nil)), nil
}
