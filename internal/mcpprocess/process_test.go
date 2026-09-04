package mcpprocess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const testReceiveTimeout = 5 * time.Second

func TestProcessInitializeToolsListAndEcho(t *testing.T) {
	p := newFakeProcess(t, 64*1024)
	startProcess(t, p)

	tests := []struct {
		name     string
		request  string
		expected string
	}{
		{
			name:     "initialize",
			request:  `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			expected: `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"test"}}`,
		},
		{
			name:     "tools list",
			request:  `{"jsonrpc":"2.0","id":"list-2","method":"tools/list","params":{}}`,
			expected: `{"jsonrpc":"2.0","id":"list-2","result":{"tools":[{"name":"echo","description":"Echoes text","annotations":{"readOnlyHint":true},"inputSchema":{"type":"object","required":["text"],"properties":{"text":{"type":"string"}}}}]}}`,
		},
		{
			name:     "echo tool call",
			request:  `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hello from test"}}}`,
			expected: `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"hello from test"}]}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := p.Send(t.Context(), json.RawMessage(tt.request)); err != nil {
				t.Fatalf("Send() error = %v", err)
			}
			response := receiveResponse(t, p.Responses())
			assertJSONEqual(t, tt.expected, response)
		})
	}
}

func TestProcessSeparatesStderrFromProtocol(t *testing.T) {
	p := newFakeProcess(t, 64*1024)
	startProcess(t, p)

	request := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if err := p.Send(t.Context(), request); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	assertJSONEqual(t, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"test"}}`, receiveResponse(t, p.Responses()))
	diagnostic := receiveDiagnostic(t, p.Diagnostics())
	if !strings.Contains(diagnostic.Message, "fake diagnostic: child started") {
		t.Fatalf("diagnostic message = %q, want fake child diagnostic", diagnostic.Message)
	}
	if json.Valid([]byte(diagnostic.Message)) {
		t.Fatalf("stderr diagnostic unexpectedly appeared as JSON protocol: %q", diagnostic.Message)
	}
}

func TestProcessRejectsSendBeforeStart(t *testing.T) {
	p := newFakeProcess(t, 64*1024)

	err := p.Send(t.Context(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err == nil {
		t.Fatal("Send() error = nil, want not-started error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "start") {
		t.Fatalf("Send() error = %q, want message identifying process is not started", err)
	}
}

func TestProcessRejectsOversizedResponseFrame(t *testing.T) {
	const maxFrameBytes = 256
	p := newFakeProcess(t, maxFrameBytes)
	startProcess(t, p)

	request := json.RawMessage(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{"text":"x","repeat":1024}}}`)
	if err := p.Send(t.Context(), request); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	diagnostic := receiveDiagnosticContaining(t, p.Diagnostics(), "frame")
	if !strings.Contains(diagnostic.Message, "256") {
		t.Fatalf("diagnostic message = %q, want configured frame bound", diagnostic.Message)
	}
	select {
	case response := <-p.Responses():
		if len(response) > maxFrameBytes {
			t.Fatalf("oversized response of %d bytes escaped the frame bound", len(response))
		}
	case <-time.After(200 * time.Millisecond):
	}
}

func TestProcessRejectsOversizedRequestFrame(t *testing.T) {
	p := newFakeProcess(t, 128)
	startProcess(t, p)

	request := json.RawMessage(`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"echo","arguments":{"text":"` + strings.Repeat("x", 128) + `"}}}`)
	err := p.Send(t.Context(), request)
	if err == nil {
		t.Fatal("Send() error = nil, want oversized-frame error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "frame") {
		t.Fatalf("Send() error = %q, want frame-bound message", err)
	}
}

func TestProcessStopThenWaitCompletesLifecycle(t *testing.T) {
	p := newFakeProcess(t, 64*1024)
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	stopCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait() after graceful Stop() error = %v", err)
	}
	if err := p.Send(t.Context(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); err == nil {
		t.Fatal("Send() after Stop() error = nil")
	}
}

func TestProcessCancellationTerminatesChildAndUnblocksWait(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	p := newFakeProcess(t, 64*1024)
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	cancel()
	waited := make(chan error, 1)
	go func() { waited <- p.Wait() }()

	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() remained blocked after process context cancellation")
	}
}
func TestProcessStopReturnsWhenResponsesAreNotDrained(t *testing.T) {
	p := newFakeProcess(t, 64*1024)
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	var sends sync.WaitGroup
	for id := 1; id <= processQueueSize*2; id++ {
		id := id
		sends.Add(1)
		go func() {
			defer sends.Done()
			request := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{}}`, id))
			_ = p.Send(context.Background(), request)
		}()
	}
	time.Sleep(200 * time.Millisecond)

	stopDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		stopDone <- p.Stop(ctx)
	}()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("Stop() remained blocked while caller stopped draining Responses")
	}
	sends.Wait()

	waitDone := make(chan error, 1)
	go func() { waitDone <- p.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Wait() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait() remained blocked after Stop()")
	}
}

func TestProcessNaturalChildExitStopsWriterAndWaits(t *testing.T) {
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find Go executable: %v", err)
	}
	p := NewProcess(Command{Path: goPath, Args: []string{"version"}}, Options{StopTimeout: time.Second})
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- p.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Wait() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() remained blocked after natural child exit")
	}
	managed := p.(*managedProcess)
	select {
	case <-managed.shutdown:
	default:
		t.Fatal("writer shutdown signal remained open after natural child exit")
	}
}

func TestProcessStartHonorsAlreadyCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := newFakeProcess(t, 64*1024)

	err := p.Start(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context.Canceled", err)
	}
}
func TestProcessReadinessCommitRejectsAfterWaitReturns(t *testing.T) {
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find Go executable: %v", err)
	}
	p := NewProcess(Command{Path: goPath, Args: []string{"version"}}, Options{StopTimeout: time.Second})
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	called := false
	err = p.CommitReadiness(func() error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrReadinessUnavailable) {
		t.Fatalf("CommitReadiness() error = %v, want ErrReadinessUnavailable", err)
	}
	if called {
		t.Fatal("CommitReadiness() invoked callback after Wait returned")
	}
}

func TestLauncherCanonicalizesTrustedLocalCommandAndCopiesArguments(t *testing.T) {
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find Go executable: %v", err)
	}
	trustedArgs := []string{"run", "./testdata/fake-mcp"}
	launcher := NewLauncher(goPath, trustedArgs...)

	first, err := launcher.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	wantPath, err := filepath.Abs(goPath)
	if err != nil {
		t.Fatalf("canonicalize trusted executable: %v", err)
	}
	wantPath, err = filepath.EvalSymlinks(wantPath)
	if err != nil {
		t.Fatalf("resolve trusted executable links: %v", err)
	}
	pathsEqual := first.Path == wantPath
	if runtime.GOOS == "windows" {
		pathsEqual = strings.EqualFold(first.Path, wantPath)
	}
	if !pathsEqual {
		t.Fatalf("resolved path = %q, want canonical trusted path %q", first.Path, wantPath)
	}
	if strings.Join(first.Args, "\x00") != strings.Join(trustedArgs, "\x00") {
		t.Fatalf("resolved args = %#v, want %#v", first.Args, trustedArgs)
	}

	trustedArgs[0] = "remote-overwrite"
	first.Args[1] = "returned-overwrite"
	second, err := launcher.Resolve()
	if err != nil {
		t.Fatalf("second Resolve() error = %v", err)
	}
	if second.Args[0] != "run" || second.Args[1] != "./testdata/fake-mcp" {
		t.Fatalf("resolved trusted args were mutable: %#v", second.Args)
	}
}

func TestLauncherRejectsNonLocalAndControlCharacterInputs(t *testing.T) {
	tests := []struct {
		name string
		path string
		args []string
	}{
		{name: "remote URL", path: "https://example.test/mcp.exe"},
		{name: "empty path"},
		{name: "nul in path", path: "mcp\x00.exe"},
		{name: "newline in argument", path: os.Args[0], args: []string{"ok\nmalicious"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewLauncher(tt.path, tt.args...).Resolve(); err == nil {
				t.Fatal("Resolve() error = nil, want untrusted launcher input rejection")
			}
		})
	}
}

func TestLauncherUsesCOMSPECOnlyForTrustedWindowsBatchFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows launcher contract")
	}
	batchPath := filepath.Join(t.TempDir(), "official mcp.bat")
	if err := os.WriteFile(batchPath, []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatalf("write batch fixture: %v", err)
	}
	launcher := NewLauncher(batchPath)

	command, err := launcher.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	comspec, err := filepath.Abs(os.Getenv("COMSPEC"))
	if err != nil {
		t.Fatalf("canonicalize COMSPEC: %v", err)
	}
	if !strings.EqualFold(command.Path, comspec) {
		t.Fatalf("batch command path = %q, want COMSPEC %q", command.Path, comspec)
	}
	wantArgs := []string{"/d", "/s", "/c", `""` + batchPath + `""`}
	if strings.Join(command.Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("batch command args = %#v, want %#v", command.Args, wantArgs)
	}
}

func TestLauncherRejectsArgumentsForWindowsBatchFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows launcher contract")
	}
	batchPath := filepath.Join(t.TempDir(), "official-mcp.bat")
	if err := os.WriteFile(batchPath, []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatalf("write batch fixture: %v", err)
	}

	if _, err := NewLauncher(batchPath, "& calc.exe").Resolve(); err == nil {
		t.Fatal("Resolve() error = nil, want batch arguments rejected so COMSPEC receives only the validated path")
	}
}

func newFakeProcess(t *testing.T, maxFrameBytes int) Process {
	t.Helper()
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find Go executable: %v", err)
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "fake-mcp")
	return NewProcess(Command{Path: goPath, Args: []string{"run", fixtureDir}}, Options{
		MaxFrameBytes: maxFrameBytes,
		StopTimeout:   2 * time.Second,
	})
}

func TestQuoteBatchPathForCOMSPEC(t *testing.T) {
	path := `C:\Program Files\Official MCP\server.bat`
	if got, want := quoteBatchPath(path), `""C:\Program Files\Official MCP\server.bat""`; got != want {
		t.Fatalf("quoteBatchPath(%q) = %q, want %q", path, got, want)
	}
}

func startProcess(t *testing.T, p Process) {
	t.Helper()
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
		_ = p.Wait()
	})
}

func receiveResponse(t *testing.T, responses <-chan json.RawMessage) json.RawMessage {
	t.Helper()
	select {
	case response, ok := <-responses:
		if !ok {
			t.Fatal("response channel closed before a response arrived")
		}
		return response
	case <-time.After(testReceiveTimeout):
		t.Fatal("timed out waiting for response")
		return nil
	}
}

func receiveDiagnostic(t *testing.T, diagnostics <-chan SafeProcessError) SafeProcessError {
	t.Helper()
	select {
	case diagnostic, ok := <-diagnostics:
		if !ok {
			t.Fatal("diagnostic channel closed before a diagnostic arrived")
		}
		return diagnostic
	case <-time.After(testReceiveTimeout):
		t.Fatal("timed out waiting for diagnostic")
		return SafeProcessError{}
	}
}

func receiveDiagnosticContaining(t *testing.T, diagnostics <-chan SafeProcessError, text string) SafeProcessError {
	t.Helper()
	timer := time.NewTimer(testReceiveTimeout)
	defer timer.Stop()
	for {
		select {
		case diagnostic, ok := <-diagnostics:
			if !ok {
				t.Fatalf("diagnostic channel closed before a message containing %q arrived", text)
			}
			if strings.Contains(strings.ToLower(diagnostic.Message), strings.ToLower(text)) {
				return diagnostic
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for diagnostic containing %q", text)
		}
	}
}

func assertJSONEqual(t *testing.T, expected string, actual json.RawMessage) {
	t.Helper()
	var want any
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatalf("invalid expected JSON: %v", err)
	}
	var got any
	if err := json.Unmarshal(actual, &got); err != nil {
		t.Fatalf("response is invalid JSON %q: %v", actual, err)
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("response = %s, want %s", gotJSON, wantJSON)
	}
}
