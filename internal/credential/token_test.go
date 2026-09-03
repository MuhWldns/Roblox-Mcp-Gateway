package credential

import (
	"crypto/subtle"
	"strings"
	"testing"
)

func TestTokenGenerateEntropyAndDigest(t *testing.T) {
	pepper := []byte("pepper")
	first, digest, err := Generate("rks_", 32, pepper)
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, err := Generate("rks_", 32, pepper)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "rks_") || !strings.HasPrefix(second, "rks_") {
		t.Fatal("missing prefix")
	}
	if first == second {
		t.Fatal("tokens unexpectedly equal")
	}
	if digest == secondDigest {
		t.Fatal("digests unexpectedly equal")
	}
	if !EqualDigest(digest, Digest(first, pepper)) {
		t.Fatal("digest mismatch")
	}
}

func TestTokenWrongPepperAndConstantTimeDigestComparison(t *testing.T) {
	plain, digest, err := Generate("rks_", 32, []byte("correct"))
	if err != nil {
		t.Fatal(err)
	}
	wrong := Digest(plain, []byte("wrong"))
	if EqualDigest(digest, wrong) {
		t.Fatal("wrong pepper accepted")
	}
	if subtle.ConstantTimeCompare(digest[:], wrong[:]) != 0 {
		t.Fatal("wrong digest compares equal")
	}
}
