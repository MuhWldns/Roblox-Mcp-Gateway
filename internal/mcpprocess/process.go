package mcpprocess

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

const (
	defaultMaxFrameBytes = 4 * 1024 * 1024
	defaultStopTimeout   = 5 * time.Second
	processQueueSize     = 16
)

type SafeProcessError struct {
	Message string
}

type Options struct {
	MaxFrameBytes int
	StopTimeout   time.Duration
}

type Process interface {
	Start(context.Context) error
	Send(context.Context, json.RawMessage) error
	Responses() <-chan json.RawMessage
	Diagnostics() <-chan SafeProcessError
	Stop(context.Context) error
	Wait() error
	// CommitReadiness atomically checks that the child is still live and runs
	// commit while holding the process lifecycle lock.
	CommitReadiness(func() error) error
}

var ErrReadinessUnavailable = errors.New("MCP process is no longer ready")

type managedProcess struct {
	command      Command
	maxFrame     int
	stopTimeout  time.Duration
	responses    chan json.RawMessage
	diagnostics  chan SafeProcessError
	writes       chan writeRequest
	stopWriter   chan struct{}
	shutdown     chan struct{}
	shutdownOnce sync.Once
	done         chan struct{}
	mu           sync.Mutex
	started      bool
	stopping     bool
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	processCtx   context.Context
	waitErr      error
	pendingMu    sync.Mutex
	pendingCalls map[string]int
}

type writeRequest struct {
	ctx    context.Context
	frame  json.RawMessage
	idKey  string
	result chan error
}

func NewProcess(command Command, options Options) Process {
	maxFrame := options.MaxFrameBytes
	if maxFrame <= 0 {
		maxFrame = defaultMaxFrameBytes
	}
	stopTimeout := options.StopTimeout
	if stopTimeout <= 0 {
		stopTimeout = defaultStopTimeout
	}
	return &managedProcess{
		command:      Command{Path: command.Path, Args: append([]string(nil), command.Args...)},
		maxFrame:     maxFrame,
		stopTimeout:  stopTimeout,
		responses:    make(chan json.RawMessage, processQueueSize),
		diagnostics:  make(chan SafeProcessError, processQueueSize),
		writes:       make(chan writeRequest, processQueueSize),
		stopWriter:   make(chan struct{}),
		shutdown:     make(chan struct{}),
		done:         make(chan struct{}),
		pendingCalls: make(map[string]int),
	}
}

func (p *managedProcess) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return errors.New("MCP process already started")
	}
	if p.command.Path == "" {
		return errors.New("MCP process command path is empty")
	}

	cmd := exec.CommandContext(ctx, p.command.Path, p.command.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open MCP stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open MCP stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("open MCP stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return fmt.Errorf("start MCP process: %w", err)
	}

	p.started = true
	p.cmd = cmd
	p.stdin = stdin
	p.processCtx = ctx

	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		p.writeLoop(ctx, stdin)
	}()
	go func() {
		defer workers.Done()
		p.readResponses(ctx, stdout)
	}()
	go func() {
		defer workers.Done()
		p.readDiagnostics(ctx, stderr)
	}()
	go p.reap(cmd, ctx, &workers)
	return nil
}

func (p *managedProcess) Send(ctx context.Context, frame json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(frame) > p.maxFrame {
		return fmt.Errorf("JSON-RPC frame exceeds %d byte limit", p.maxFrame)
	}
	if err := validateRequest(frame); err != nil {
		return err
	}

	p.mu.Lock()
	ready := p.started && !p.stopping
	processCtx := p.processCtx
	p.mu.Unlock()
	if !ready {
		return errors.New("MCP process is not started")
	}

	request := writeRequest{
		ctx:    ctx,
		frame:  append(json.RawMessage(nil), frame...),
		idKey:  requestIDKey(frame),
		result: make(chan error, 1),
	}
	select {
	case p.writes <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-processCtx.Done():
		return processCtx.Err()
	case <-p.done:
		return errors.New("MCP process has stopped")
	}
	select {
	case err := <-request.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-processCtx.Done():
		return processCtx.Err()
	case <-p.done:
		return errors.New("MCP process stopped before writing request")
	}
}

func (p *managedProcess) Responses() <-chan json.RawMessage {
	return p.responses
}

func (p *managedProcess) Diagnostics() <-chan SafeProcessError {
	return p.diagnostics
}

func (p *managedProcess) Stop(ctx context.Context) error {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return errors.New("MCP process is not started")
	}
	if !p.stopping {
		p.stopping = true
		close(p.stopWriter)
		p.shutdownOnce.Do(func() { close(p.shutdown) })
	}
	cmd := p.cmd
	p.mu.Unlock()

	timer := time.NewTimer(p.stopTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return p.stopResult()
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-p.done
		return ctx.Err()
	case <-timer.C:
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-p.done
		return errors.New("MCP process graceful stop deadline exceeded")
	}
}

func (p *managedProcess) Wait() error {
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if !started {
		return errors.New("MCP process is not started")
	}
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func (p *managedProcess) stopResult() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.processCtx != nil && p.processCtx.Err() != nil {
		return p.processCtx.Err()
	}
	return p.waitErr
}
func (p *managedProcess) CommitReadiness(commit func() error) error {
	if commit == nil {
		return errors.New("readiness commit callback is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started || p.stopping {
		return ErrReadinessUnavailable
	}
	return commit()
}

func (p *managedProcess) writeLoop(ctx context.Context, stdin io.WriteCloser) {
	writer := bufio.NewWriter(stdin)
	defer stdin.Close()
	for {
		select {
		case <-ctx.Done():
		case <-p.shutdown:
			return
		case request := <-p.writes:
			if request.ctx == nil {
				continue
			}
			if err := request.ctx.Err(); err != nil {
				request.result <- err
				continue
			}
			if request.idKey != "" {
				p.addPending(request.idKey)
			}
			err := writeFrame(writer, request.frame)
			if err != nil && request.idKey != "" {
				p.removePending(request.idKey)
			}
			request.result <- err
			if err != nil {
				p.emitDiagnostic(fmt.Sprintf("MCP stdin write failed: %v", err))
				return
			}
		}
	}
}

func writeFrame(writer *bufio.Writer, frame json.RawMessage) error {
	if _, err := writer.Write(frame); err != nil {
		return err
	}
	if err := writer.WriteByte('\n'); err != nil {
		return err
	}
	return writer.Flush()
}

func (p *managedProcess) readResponses(ctx context.Context, stdout io.Reader) {
	p.scanFrames(stdout, func(frame []byte) {
		if err := p.validateIncoming(frame); err != nil {
			p.emitDiagnostic(fmt.Sprintf("invalid MCP protocol frame: %v", err))
			return
		}
		response := append(json.RawMessage(nil), frame...)
		select {
		case p.responses <- response:
		case <-ctx.Done():
		case <-p.shutdown:
		}
	}, "stdout")
}

func (p *managedProcess) readDiagnostics(ctx context.Context, stderr io.Reader) {
	p.scanFrames(stderr, func(frame []byte) {
		select {
		case p.diagnostics <- SafeProcessError{Message: string(frame)}:
		case <-ctx.Done():
		default:
		}
	}, "stderr")
}

func (p *managedProcess) scanFrames(reader io.Reader, consume func([]byte), stream string) {
	scanner := bufio.NewScanner(reader)
	capacity := p.maxFrame + 2
	if capacity < 0 {
		capacity = p.maxFrame
	}
	scanner.Buffer(make([]byte, min(capacity, 64*1024)), capacity)
	for scanner.Scan() {
		frame := scanner.Bytes()
		if len(frame) > p.maxFrame {
			p.emitDiagnostic(fmt.Sprintf("MCP %s frame exceeds %d byte limit", stream, p.maxFrame))
			return
		}
		consume(frame)
	}
	if err := scanner.Err(); err != nil {
		p.emitDiagnostic(fmt.Sprintf("MCP %s frame exceeds %d byte limit: %v", stream, p.maxFrame, err))
	}
}

func (p *managedProcess) validateIncoming(frame json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(frame, &object); err != nil {
		return err
	}
	if _, isRequest := object["method"]; isRequest {
		return validateRequest(frame)
	}
	id, ok := object["id"]
	if !ok {
		return errors.New("JSON-RPC response is missing id")
	}
	key, err := idKey(id)
	if err != nil {
		return err
	}
	if !p.hasPending(key) {
		return fmt.Errorf("JSON-RPC response id %s has no pending request", id)
	}
	if err := validateResponse(frame, id); err != nil {
		return err
	}
	p.removePending(key)
	return nil
}

func (p *managedProcess) reap(cmd *exec.Cmd, ctx context.Context, workers *sync.WaitGroup) {
	err := cmd.Wait()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	// Publish the child exit before waiting for protocol workers to drain. This
	// closes the readiness commit window as soon as Wait returns; done and
	// response-channel closure remain deferred until worker cleanup completes.
	p.mu.Lock()
	p.waitErr = err
	p.stopping = true
	p.mu.Unlock()
	p.shutdownOnce.Do(func() { close(p.shutdown) })
	workers.Wait()
	close(p.responses)
	close(p.diagnostics)
	close(p.done)
}

func (p *managedProcess) emitDiagnostic(message string) {
	select {
	case p.diagnostics <- SafeProcessError{Message: message}:
	default:
	}
}

func requestIDKey(frame json.RawMessage) string {
	var object map[string]json.RawMessage
	if json.Unmarshal(frame, &object) != nil {
		return ""
	}
	key, _ := idKey(object["id"])
	return key
}

func idKey(id json.RawMessage) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	var text string
	if json.Unmarshal(id, &text) == nil {
		return "s:" + text, nil
	}
	return "n:" + string(id), nil
}

func (p *managedProcess) addPending(key string) {
	p.pendingMu.Lock()
	p.pendingCalls[key]++
	p.pendingMu.Unlock()
}

func (p *managedProcess) hasPending(key string) bool {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	return p.pendingCalls[key] > 0
}

func (p *managedProcess) removePending(key string) {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	if p.pendingCalls[key] <= 1 {
		delete(p.pendingCalls, key)
		return
	}
	p.pendingCalls[key]--
}

var _ = strconv.IntSize
