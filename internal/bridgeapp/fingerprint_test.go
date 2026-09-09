package bridgeapp

import (
	"regexp"
	"testing"
)

var hex64Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestDeriveMachineFingerprintHash_KnownDeterministicVectors verifies that
// deriveMachineFingerprintHash computes the exact expected 64-character lowercase
// hex HMAC-SHA256 digests. This guarantees that unkeyed hashing (e.g. raw SHA-256),
// key changes, label changes, or formatting regressions immediately fail.
func TestDeriveMachineFingerprintHash_KnownDeterministicVectors(t *testing.T) {
	tests := []struct {
		name         string
		machineGUID  string
		productUUID  string
		volumeSerial string
		wantHash     string
	}{
		{
			name:         "all three sources present",
			machineGUID:  "3177695c-9c02-45e6-9b51-89be0e74f88e",
			productUUID:  "58334444-1234-5678-9abc-def012345678",
			volumeSerial: "8c12a34f",
			wantHash:     "546b25d04d55bfb77a228d60708d2b85e55232e825bd8cfa752c281cde0b6845",
		},
		{
			name:         "guid and product uuid (missing volume serial)",
			machineGUID:  "3177695c-9c02-45e6-9b51-89be0e74f88e",
			productUUID:  "58334444-1234-5678-9abc-def012345678",
			volumeSerial: "",
			wantHash:     "aede7ae586549ab6c552b493bdd5ae372dbde508b0dd543fb5c1ecb80448d1e0",
		},
		{
			name:         "guid and volume serial (missing product uuid)",
			machineGUID:  "3177695c-9c02-45e6-9b51-89be0e74f88e",
			productUUID:  "",
			volumeSerial: "8c12a34f",
			wantHash:     "fd0f32968c7e62a289da1dcc5aac6b5c318ca6d3cab474757e39de0daa5e6892",
		},
		{
			name:         "product uuid and volume serial (missing guid)",
			machineGUID:  "",
			productUUID:  "58334444-1234-5678-9abc-def012345678",
			volumeSerial: "8c12a34f",
			wantHash:     "a08047e5fdd53276c2c972fc352e7c5215e27a40333ace93a26d4c97f351e7cb",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deriveMachineFingerprintHash(tc.machineGUID, tc.productUUID, tc.volumeSerial)
			if err != nil {
				t.Fatalf("deriveMachineFingerprintHash(%q, %q, %q) returned unexpected error: %v",
					tc.machineGUID, tc.productUUID, tc.volumeSerial, err)
			}
			if !hex64Pattern.MatchString(got) {
				t.Errorf("got %q, want 64-character lowercase hex string", got)
			}
			if got != tc.wantHash {
				t.Errorf("got %q, want %q", got, tc.wantHash)
			}
		})
	}
}

// TestDeriveMachineFingerprintHash_Normalization verifies that case differences
// and surrounding whitespace across any source are normalized so that identical
// physical identities produce identical digests.
func TestDeriveMachineFingerprintHash_Normalization(t *testing.T) {
	const canonicalHash = "546b25d04d55bfb77a228d60708d2b85e55232e825bd8cfa752c281cde0b6845"

	tests := []struct {
		name         string
		machineGUID  string
		productUUID  string
		volumeSerial string
	}{
		{
			name:         "canonical lowercase clean",
			machineGUID:  "3177695c-9c02-45e6-9b51-89be0e74f88e",
			productUUID:  "58334444-1234-5678-9abc-def012345678",
			volumeSerial: "8c12a34f",
		},
		{
			name:         "all uppercase",
			machineGUID:  "3177695C-9C02-45E6-9B51-89BE0E74F88E",
			productUUID:  "58334444-1234-5678-9ABC-DEF012345678",
			volumeSerial: "8C12A34F",
		},
		{
			name:         "mixed case with leading and trailing spaces",
			machineGUID:  "  3177695c-9C02-45e6-9B51-89be0e74f88e  ",
			productUUID:  " 58334444-1234-5678-9abc-DEF012345678 ",
			volumeSerial: "  8C12a34f   ",
		},
		{
			name:         "tabs and newline whitespace",
			machineGUID:  "\t3177695C-9C02-45E6-9B51-89BE0E74F88E\n",
			productUUID:  "\r\n58334444-1234-5678-9abc-def012345678\t",
			volumeSerial: "\n  8c12a34f  \r\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deriveMachineFingerprintHash(tc.machineGUID, tc.productUUID, tc.volumeSerial)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != canonicalHash {
				t.Errorf("normalized hash mismatch: got %q, want canonical %q", got, canonicalHash)
			}
		})
	}
}

// TestDeriveMachineFingerprintHash_SourceLabelsAndOrderDistinctness verifies that
// source identity labels and stable source ordering prevent collisions when the same
// raw values appear in different source slots.
func TestDeriveMachineFingerprintHash_SourceLabelsAndOrderDistinctness(t *testing.T) {
	const (
		valA = "ident-val-alpha"
		valB = "ident-val-beta"
	)

	// Permutations of identical values across distinct source slots must produce
	// mutually distinct hashes due to field labeling and ordering.
	cases := []struct {
		name         string
		machineGUID  string
		productUUID  string
		volumeSerial string
	}{
		{
			name:         "guid=A, uuid=B",
			machineGUID:  valA,
			productUUID:  valB,
			volumeSerial: "",
		},
		{
			name:         "guid=B, uuid=A (swapped values)",
			machineGUID:  valB,
			productUUID:  valA,
			volumeSerial: "",
		},
		{
			name:         "guid=A, serial=B",
			machineGUID:  valA,
			productUUID:  "",
			volumeSerial: valB,
		},
		{
			name:         "guid=B, serial=A",
			machineGUID:  valB,
			productUUID:  "",
			volumeSerial: valA,
		},
		{
			name:         "uuid=A, serial=B",
			machineGUID:  "",
			productUUID:  valA,
			volumeSerial: valB,
		},
		{
			name:         "uuid=B, serial=A",
			machineGUID:  "",
			productUUID:  valB,
			volumeSerial: valA,
		},
	}

	seen := make(map[string]string)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deriveMachineFingerprintHash(tc.machineGUID, tc.productUUID, tc.volumeSerial)
			if err != nil {
				t.Fatalf("unexpected derivation error: %v", err)
			}
			if !hex64Pattern.MatchString(got) {
				t.Fatalf("hash %q is not 64-char lowercase hex", got)
			}
			if prevName, exists := seen[got]; exists {
				t.Fatalf("hash collision between %q and %q (hash: %q)", prevName, tc.name, got)
			}
			seen[got] = tc.name
		})
	}
}

// TestDeriveMachineFingerprintHash_FewerThanTwoSourcesFails verifies the fail-closed
// requirement: at least two distinct, non-empty, non-whitespace sources are required
// to derive a valid hardware fingerprint. With fewer than two sources, derivation fails
// with an error and returns an empty hash.
func TestDeriveMachineFingerprintHash_FewerThanTwoSourcesFails(t *testing.T) {
	tests := []struct {
		name         string
		machineGUID  string
		productUUID  string
		volumeSerial string
	}{
		{
			name:         "all empty strings",
			machineGUID:  "",
			productUUID:  "",
			volumeSerial: "",
		},
		{
			name:         "all whitespace strings",
			machineGUID:  "   ",
			productUUID:  "\t\n",
			volumeSerial: " \r\n ",
		},
		{
			name:         "only guid provided",
			machineGUID:  "3177695c-9c02-45e6-9b51-89be0e74f88e",
			productUUID:  "",
			volumeSerial: "",
		},
		{
			name:         "only product uuid provided",
			machineGUID:  "",
			productUUID:  "58334444-1234-5678-9abc-def012345678",
			volumeSerial: "",
		},
		{
			name:         "only volume serial provided",
			machineGUID:  "",
			productUUID:  "",
			volumeSerial: "8c12a34f",
		},
		{
			name:         "guid with whitespace-only second and third sources",
			machineGUID:  "3177695c-9c02-45e6-9b51-89be0e74f88e",
			productUUID:  "   ",
			volumeSerial: "\t\r\n",
		},
		{
			name:         "product uuid with whitespace-only first and third sources",
			machineGUID:  " \t ",
			productUUID:  "58334444-1234-5678-9abc-def012345678",
			volumeSerial: "   ",
		},
		{
			name:         "volume serial with whitespace-only first and second sources",
			machineGUID:  "  ",
			productUUID:  "\t\n\r",
			volumeSerial: "8c12a34f",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deriveMachineFingerprintHash(tc.machineGUID, tc.productUUID, tc.volumeSerial)
			if err == nil {
				t.Fatalf("deriveMachineFingerprintHash(%q, %q, %q) = %q, want non-nil error",
					tc.machineGUID, tc.productUUID, tc.volumeSerial, got)
			}
			if got != "" {
				t.Errorf("got hash %q on error, want empty string", got)
			}
		})
	}
}
