// Package scripts pins the final E2E gate contract in source and behavior
// form. The gate is the release-blocking proof, so it must be STRICT: it
// refuses to run the live matrix without a configured MySQL test DSN and can
// therefore never print PASS on an unconfigured machine.
package scripts

import (
	"bytes"
	"os"
	"os/exec"
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
