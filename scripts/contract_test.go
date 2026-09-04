// Package scripts pins the final E2E gate contract in source and behavior
// form. The gate is the release-blocking proof, so it must be STRICT: it
// refuses to run the live matrix without a configured MySQL test DSN and can
// therefore never print PASS on an unconfigured machine.
package scripts

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const gateScriptPath = "e2e-matrix.ps1"

func gateSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(gateScriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", gateScriptPath, err)
	}
	return string(raw)
}

// The DSN guard must exist and must run BEFORE anything else that could
// execute a test aggregate — otherwise a blank DSN silently degrades the
// release gate into a green lie.
func TestGateRefusesToRunWithoutDSN(t *testing.T) {
	source := gateSource(t)
	guardIdx := strings.Index(source, "MYSQL_TEST_DSN")
	if guardIdx < 0 {
		t.Fatal("the gate must check MYSQL_TEST_DSN explicitly")
	}
	failIdx := strings.Index(source, "gate: FAIL")
	if failIdx < 0 || failIdx < guardIdx {
		t.Fatal("a missing/blank MYSQL_TEST_DSN must fail the gate with a clear message")
	}
	exitIdx := strings.Index(source, "exit 2")
	if exitIdx < 0 {
		t.Fatal("a missing/blank MYSQL_TEST_DSN must exit the gate non-zero")
	}
	firstAggregate := strings.Index(source, "Invoke-GateTest")
	if firstAggregate < 0 {
		t.Fatal("the gate must run its aggregates through Invoke-GateTest")
	}
	if !(failIdx < firstAggregate && exitIdx < firstAggregate) {
		t.Fatalf("the DSN guard (fail@%d exit@%d) must precede every aggregate (@%d)", failIdx, exitIdx, firstAggregate)
	}
}

// The behavioral half: with MYSQL_TEST_DSN absent from the environment the
// gate must exit non-zero and must never print PASS. This is the proof that
// an unconfigured machine cannot produce a false green gate.
func TestGateRunWithoutDSNNeverPrintsPass(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the gate is a Windows PowerShell script")
	}
	if _, err := exec.LookPath("powershell"); err != nil {
		t.Skip("powershell is unavailable")
	}
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", gateScriptPath)
	// A scrubbed environment: no MYSQL_TEST_DSN of any value reaches the gate.
	cmd.Env = []string{"SystemRoot=" + os.Getenv("SystemRoot"), "PATH=" + os.Getenv("PATH")}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err == nil {
		t.Fatalf("the gate must exit non-zero without MYSQL_TEST_DSN; output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "gate: PASS") {
		t.Fatalf("the gate printed PASS without MYSQL_TEST_DSN; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "gate: FAIL") {
		t.Fatalf("the gate must explain the refusal with a clear message; output:\n%s", out.String())
	}
}

func writeReleaseFixture(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatalf("create fixture dist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "index.html"), []byte("release fixture\n"), 0o644); err != nil {
		t.Fatalf("write fixture artifact: %v", err)
	}
	if manifest == "generated" {
		hash := sha256.Sum256([]byte("release fixture\n"))
		manifest = fmt.Sprintf("%x *dist/index.html\n", hash)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA-256SUMS"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write fixture manifest: %v", err)
	}
	return dir
}

func runReleaseVerification(t *testing.T, releaseDir string) (string, error) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("the release verifier is a Windows PowerShell script")
	}
	if _, err := exec.LookPath("powershell"); err != nil {
		t.Skip("powershell is unavailable")
	}
	path, err := filepath.Abs("smoke-vps.ps1")
	if err != nil {
		t.Fatalf("resolve smoke script: %v", err)
	}
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path,
		"-VerifyReleaseOnly", "-ReleaseDirectory", releaseDir)
	cmd.Env = []string{"SystemRoot=" + os.Getenv("SystemRoot"), "PATH=" + os.Getenv("PATH")}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return out.String(), err
}

func TestReleaseVerifierAcceptsGeneratedGNUBinaryManifest(t *testing.T) {
	dir := writeReleaseFixture(t, "generated")
	out, err := runReleaseVerification(t, dir)
	if err != nil {
		t.Fatalf("generated GNU binary manifest must verify: %v\n%s", err, out)
	}
	if !strings.Contains(out, "release artifacts match SHA-256SUMS") {
		t.Fatalf("verification did not report success:\n%s", out)
	}
}

func TestReleaseVerifierRejectsUnsafeOrMalformedManifestEntries(t *testing.T) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("release fixture\n")))
	tests := []struct {
		name     string
		manifest string
	}{
		{name: "text mode delimiter", manifest: hash + "  dist/index.html\n"},
		{name: "short digest", manifest: strings.Repeat("a", 63) + " *dist/index.html\n"},
		{name: "uppercase digest", manifest: strings.ToUpper(hash) + " *dist/index.html\n"},
		{name: "parent traversal", manifest: hash + " *../outside\n"},
		{name: "nested parent traversal", manifest: hash + " *dist/../../outside\n"},
		{name: "posix absolute", manifest: hash + " */etc/passwd\n"},
		{name: "windows absolute", manifest: hash + " *C:/Windows/system.ini\n"},
		{name: "backslash path", manifest: hash + " *dist\\index.html\n"},
		{name: "duplicate path", manifest: hash + " *dist/index.html\n" + hash + " *dist/index.html\n"},
		{name: "blank line", manifest: hash + " *dist/index.html\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeReleaseFixture(t, tt.manifest)
			out, err := runReleaseVerification(t, dir)
			if err == nil {
				t.Fatalf("unsafe manifest unexpectedly verified:\n%s", out)
			}
		})
	}
}

func TestReleaseVerifierChecksFilesAndDigests(t *testing.T) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("release fixture\n")))
	tests := []struct {
		name     string
		manifest string
	}{
		{name: "missing file", manifest: hash + " *missing.bin\n"},
		{name: "wrong digest", manifest: strings.Repeat("0", 64) + " *dist/index.html\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeReleaseFixture(t, tt.manifest)
			out, err := runReleaseVerification(t, dir)
			if err == nil {
				t.Fatalf("unverifiable artifact unexpectedly passed:\n%s", out)
			}
		})
	}
}
