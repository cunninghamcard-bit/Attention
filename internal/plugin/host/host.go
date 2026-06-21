package host

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed hostjs/host.js
var embeddedHostJS string

// ErrHostDown reports that the JS plugin host is not currently available.
var ErrHostDown = errors.New("plugin host down")

// Options configures the JS host manager.
type Options struct {
	NodePath    string
	WorkDir     string
	OnEvent     func(pluginID, owner, sessionID, name string, payload []byte) error
	OnUIRequest func(UIRequest) (requestID string, err error)
	Logger      *slog.Logger
}

// ActivateSpec identifies one plugin instance to mount in the JS host.
type ActivateSpec struct {
	PluginID   string
	Owner      string
	SessionID  string
	EnvID      string
	ModulePath string
	CtxSeed    json.RawMessage
}

// MountKey is the stable identity of a mounted plugin instance.
type MountKey struct {
	PluginID  string
	Owner     string
	SessionID string
	EnvID     string
}

// Registered mirrors the registered wire frame payload.
type Registered struct {
	Tools    []ToolReg
	Hooks    []HookReg
	Commands []CommandReg
	IsError  bool
	Error    string
}

// Manager owns a single JS host process and its replayable mount table.
type Manager struct {
	opts   Options
	logger *slog.Logger

	writeMu sync.Mutex
	mu      sync.Mutex

	started          bool
	stopping         bool
	down             bool
	restartScheduled bool
	nodePath         string
	scriptPath       string
	cmd              *exec.Cmd
	stdin            io.WriteCloser
	waitCh           <-chan error
	stopCh           chan struct{}
	generation       uint64
	restartWG        sync.WaitGroup // Stop() 等在飞的重启循环退出，杜绝关停后 replayMounts 仍 emit

	pending map[string]chan rpcResult
	mounts  map[string]*mountEntry
	nextSeq uint64
	corrSeq atomic.Uint64
}

type rpcResult struct {
	frame Frame
	err   error
}

type mountEntry struct {
	key  MountKey
	spec ActivateSpec
	seq  uint64
}

type hookDispatchPayload struct {
	Index int             `json:"index"`
	Event json.RawMessage `json:"event"`
}

// New constructs a Manager. Start must be called before use.
func New(opts Options) *Manager {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		opts:    opts,
		logger:  logger,
		down:    true,
		stopCh:  make(chan struct{}),
		pending: make(map[string]chan rpcResult),
		mounts:  make(map[string]*mountEntry),
	}
}

// Start writes the embedded JS host to a temp file and starts node.
func (m *Manager) Start(ctx context.Context) error {
	nodePath := m.opts.NodePath
	if nodePath == "" {
		var err error
		nodePath, err = exec.LookPath("node")
		if err != nil {
			return fmt.Errorf("find node: %w", err)
		}
	}

	scriptPath, err := writeHostScript()
	if err != nil {
		return err
	}

	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		_ = os.Remove(scriptPath)
		return nil
	}
	m.started = true
	m.down = true
	m.nodePath = nodePath
	m.scriptPath = scriptPath
	m.mu.Unlock()

	if err := m.startProcess(ctx); err != nil {
		m.mu.Lock()
		m.started = false
		m.down = true
		m.scriptPath = ""
		m.mu.Unlock()
		_ = os.Remove(scriptPath)
		return err
	}
	return nil
}

// Stop asks the host to exit, then kills it if it does not stop promptly.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	if !m.stopping {
		m.stopping = true
		close(m.stopCh)
	}
	stdin := m.stdin
	cmd := m.cmd
	waitCh := m.waitCh
	scriptPath := m.scriptPath
	m.down = true
	m.stdin = nil
	m.failPendingLocked(ErrHostDown)
	m.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}

	if waitCh != nil {
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-waitCh:
		case <-timer.C:
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			select {
			case <-waitCh:
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// 等在飞的重启循环收敛后再删脚本：stopping+stopCh 已置位，restartLoop
	// 的 timer-select 与 startProcess/replayMounts 守卫都会快速退出，Wait
	// 保证关停返回后不再有 replayMounts 的 emit 写入会话日志。
	m.restartWG.Wait()

	if scriptPath != "" {
		_ = os.Remove(scriptPath)
	}
	return nil
}

// Activate mounts one plugin instance and records successful mounts for replay.
func (m *Manager) Activate(ctx context.Context, spec ActivateSpec) (Registered, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	frame, err := m.call(callCtx, activateFrame(spec))
	if err != nil {
		return Registered{}, err
	}
	if frame.T != FrameRegistered {
		return Registered{}, fmt.Errorf("activate returned %q, want %q", frame.T, FrameRegistered)
	}

	registered := registeredFromFrame(frame)
	if registered.IsError {
		return registered, fmt.Errorf("activate plugin %q owner %q: %s", spec.PluginID, spec.Owner, registered.Error)
	}

	m.recordMount(spec)
	return registered, nil
}

// Dispose unmounts one plugin instance and removes it from replay.
func (m *Manager) Dispose(ctx context.Context, key MountKey) error {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	frame, err := m.call(callCtx, Frame{
		T:         FrameDispose,
		PluginID:  key.PluginID,
		Owner:     key.Owner,
		SessionID: key.SessionID,
		EnvID:     key.EnvID,
	})
	if err != nil {
		return err
	}
	if frame.T != FrameDisposed {
		return fmt.Errorf("dispose returned %q, want %q", frame.T, FrameDisposed)
	}
	if frame.IsError {
		return errors.New(frame.Error)
	}

	m.removeMount(key)
	return nil
}

// DispatchCommand routes an ext.command request to a registered plugin command.
func (m *Manager) DispatchCommand(
	ctx context.Context,
	pluginID string,
	owner string,
	sessionID string,
	envID string,
	name string,
	payload []byte,
) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	frame, err := m.call(callCtx, Frame{
		T:         FrameCommandDispatch,
		PluginID:  pluginID,
		Owner:     owner,
		SessionID: sessionID,
		EnvID:     envID,
		Name:      name,
		Payload:   cloneRaw(payload),
	})
	if err != nil {
		return nil, err
	}
	if frame.T != FrameCommandDone {
		return nil, fmt.Errorf("command dispatch returned %q, want %q", frame.T, FrameCommandDone)
	}
	if frame.IsError {
		return nil, errors.New(frame.Error)
	}
	return cloneRaw(frame.Payload), nil
}

// DispatchHook routes one hook event to one registered JS hook handler index.
func (m *Manager) DispatchHook(
	ctx context.Context,
	pluginID string,
	owner string,
	sessionID string,
	point string,
	index int,
	event []byte,
) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	payload, err := json.Marshal(hookDispatchPayload{
		Index: index,
		Event: cloneRaw(event),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal hook dispatch payload: %w", err)
	}
	frame, err := m.call(callCtx, Frame{
		T:         FrameHookDispatch,
		PluginID:  pluginID,
		Owner:     owner,
		SessionID: sessionID,
		Name:      point,
		Payload:   payload,
	})
	if err != nil {
		return nil, err
	}
	if frame.T != FrameHookResult {
		return nil, fmt.Errorf("hook dispatch returned %q, want %q", frame.T, FrameHookResult)
	}
	if frame.IsError {
		return nil, errors.New(frame.Error)
	}
	return cloneRaw(frame.Payload), nil
}

// ExecuteTool routes a session-scoped tool invocation to the JS host.
func (m *Manager) ExecuteTool(
	ctx context.Context,
	pluginID string,
	owner string,
	sessionID string,
	name string,
	args []byte,
) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	frame, err := m.call(callCtx, Frame{
		T:         FrameToolExecute,
		PluginID:  pluginID,
		Owner:     owner,
		SessionID: sessionID,
		Name:      name,
		Payload:   cloneRaw(args),
	})
	if err != nil {
		return nil, err
	}
	if frame.T != FrameToolResult {
		return nil, fmt.Errorf("tool execute returned %q, want %q", frame.T, FrameToolResult)
	}
	if frame.IsError {
		return nil, errors.New(frame.Error)
	}
	return cloneRaw(frame.Payload), nil
}

// ResolveUI delivers a ui.resolved fact back into all mounted session plugin
// instances. The JS side keeps pending promises per instance and ignores
// request IDs it does not own, so this does not create an engine pending table.
func (m *Manager) ResolveUI(ctx context.Context, sessionID string, payload []byte) error {
	frames := m.uiResolvedFrames(sessionID, payload)
	for _, frame := range frames {
		if err := m.send(ctx, frame); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) uiResolvedFrames(sessionID string, payload []byte) []Frame {
	m.mu.Lock()
	defer m.mu.Unlock()

	keys := make([]string, 0, len(m.mounts))
	for key, entry := range m.mounts {
		if entry.key.SessionID == sessionID {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	frames := make([]Frame, 0, len(keys))
	for _, key := range keys {
		entry := m.mounts[key]
		frames = append(frames, Frame{
			T:         FrameUIResolved,
			PluginID:  entry.key.PluginID,
			Owner:     entry.key.Owner,
			SessionID: entry.key.SessionID,
			EnvID:     entry.key.EnvID,
			Payload:   cloneRaw(payload),
		})
	}
	return frames
}

func (m *Manager) call(ctx context.Context, frame Frame) (Frame, error) {
	if frame.CorrID == "" {
		frame.CorrID = m.nextCorrID()
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return Frame{}, fmt.Errorf("encode frame %q: %w", frame.T, err)
	}
	encoded = append(encoded, '\n')

	resultCh := make(chan rpcResult, 1)

	m.mu.Lock()
	if !m.started || m.stopping || m.down || m.stdin == nil {
		m.mu.Unlock()
		return Frame{}, ErrHostDown
	}
	stdin := m.stdin
	gen := m.generation
	m.pending[frame.CorrID] = resultCh
	m.mu.Unlock()

	m.writeMu.Lock()
	_, err = stdin.Write(encoded)
	m.writeMu.Unlock()
	if err != nil {
		m.removePending(frame.CorrID)
		m.handleHostDown(gen, err)
		return Frame{}, ErrHostDown
	}

	select {
	case result := <-resultCh:
		return result.frame, result.err
	case <-ctx.Done():
		m.removePending(frame.CorrID)
		return Frame{}, ctx.Err()
	}
}

func (m *Manager) send(ctx context.Context, frame Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode frame %q: %w", frame.T, err)
	}
	encoded = append(encoded, '\n')

	m.mu.Lock()
	if !m.started || m.stopping || m.down || m.stdin == nil {
		m.mu.Unlock()
		return ErrHostDown
	}
	stdin := m.stdin
	gen := m.generation
	m.mu.Unlock()

	m.writeMu.Lock()
	_, err = stdin.Write(encoded)
	m.writeMu.Unlock()
	if err != nil {
		m.handleHostDown(gen, err)
		return ErrHostDown
	}
	return nil
}

func (m *Manager) processKillForTest() error {
	m.mu.Lock()
	if !m.started || m.cmd == nil || m.cmd.Process == nil || m.down {
		m.mu.Unlock()
		return ErrHostDown
	}
	cmd := m.cmd
	gen := m.generation
	m.mu.Unlock()

	err := cmd.Process.Kill()
	m.handleHostDown(gen, ErrHostDown)
	return err
}

func writeHostScript() (string, error) {
	file, err := os.CreateTemp("", "along-plugin-host-*.mjs")
	if err != nil {
		return "", fmt.Errorf("create host temp file: %w", err)
	}
	path := file.Name()
	if _, err := file.WriteString(embeddedHostJS); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write host temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close host temp file: %w", err)
	}
	return path, nil
}

func (m *Manager) startProcess(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return ErrHostDown
	}
	nodePath := m.nodePath
	scriptPath := m.scriptPath
	workDir := m.opts.WorkDir
	m.mu.Unlock()

	cmd := exec.Command(nodePath, scriptPath)
	if workDir != "" {
		cmd.Dir = workDir
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open host stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open host stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open host stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("start node host: %w", err)
	}

	waitCh := make(chan error, 1)

	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ErrHostDown
	}
	m.generation++
	gen := m.generation
	m.cmd = cmd
	m.stdin = stdin
	m.waitCh = waitCh
	m.down = false
	m.restartScheduled = false
	m.mu.Unlock()

	go m.readLoop(stdout, gen)
	go m.stderrLoop(stderr)
	go m.waitLoop(cmd, gen, waitCh)
	go m.heartbeatLoop(gen)
	return nil
}

func (m *Manager) readLoop(stdout io.Reader, gen uint64) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var frame Frame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			m.logger.Error("decode host frame", "error", err)
			continue
		}
		m.processFrame(frame)
	}
	if err := scanner.Err(); err != nil {
		m.logger.Error("read host stdout", "generation", gen, "error", err)
	}
}

func (m *Manager) stderrLoop(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 8*1024), 1024*1024)
	for scanner.Scan() {
		m.logger.Debug("host stderr", "line", scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		m.logger.Debug("host stderr read failed", "error", err)
	}
}

func (m *Manager) waitLoop(cmd *exec.Cmd, gen uint64, waitCh chan<- error) {
	err := cmd.Wait()
	waitCh <- err
	close(waitCh)
	m.handleHostDown(gen, err)
}

func (m *Manager) heartbeatLoop(gen uint64) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	missed := 0
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			frame, err := m.call(ctx, Frame{T: FramePing})
			cancel()
			if err != nil || frame.T != FramePong {
				missed++
			} else {
				missed = 0
			}
			if missed >= 2 {
				m.killGeneration(gen)
				return
			}
		case <-m.stopCh:
			return
		}
	}
}

func (m *Manager) processFrame(frame Frame) {
	switch frame.T {
	case FrameEventsEmit:
		m.handleEvent(frame)
	case FrameUIRequest:
		m.handleUIRequest(frame)
	case FrameLog:
		m.handleLog(frame)
	case FrameFatal:
		m.logger.Error("host fatal", "pluginID", frame.PluginID, "owner", frame.Owner, "error", frame.Error)
	case FrameRegistered, FrameDisposed, FramePong, FrameToolResult, FrameHookResult, FrameCommandDone:
		if !m.completePending(frame) {
			m.logger.Warn("unmatched host response", "type", frame.T, "corrId", frame.CorrID)
		}
	default:
		if frame.CorrID != "" {
			if !m.completePending(frame) {
				m.logger.Warn("unmatched host frame", "type", frame.T, "corrId", frame.CorrID)
			}
			return
		}
		m.logger.Warn("unknown host frame", "type", frame.T)
	}
}

func (m *Manager) handleUIRequest(frame Frame) {
	if m.opts.OnUIRequest == nil {
		m.logger.Warn("plugin ui.request dropped: handler not configured", "pluginID", frame.PluginID, "owner", frame.Owner)
		if frame.CorrID != "" {
			m.sendUIRequestError(frame, errors.New("plugin ui.request handler not configured"))
		}
		return
	}
	requestID, err := m.opts.OnUIRequest(UIRequest{
		CorrID:    frame.CorrID,
		PluginID:  frame.PluginID,
		Owner:     frame.Owner,
		SessionID: frame.SessionID,
		EnvID:     frame.EnvID,
		Kind:      frame.Name,
		Payload:   cloneRaw(frame.Payload),
	})
	if err != nil {
		m.logger.Error("plugin ui.request handler failed", "pluginID", frame.PluginID, "owner", frame.Owner, "error", err)
		if frame.CorrID != "" {
			m.sendUIRequestError(frame, err)
		}
		return
	}
	if frame.CorrID == "" || requestID == "" {
		return
	}

	payload, err := json.Marshal(struct {
		RequestID string `json:"requestId"`
	}{RequestID: requestID})
	if err != nil {
		m.logger.Error("marshal plugin ui.request ack", "pluginID", frame.PluginID, "owner", frame.Owner, "error", err)
		return
	}
	if err := m.send(context.Background(), Frame{
		T:         FrameUIRequest,
		CorrID:    frame.CorrID,
		PluginID:  frame.PluginID,
		Owner:     frame.Owner,
		SessionID: frame.SessionID,
		EnvID:     frame.EnvID,
		Payload:   payload,
	}); err != nil {
		m.logger.Error("send plugin ui.request ack", "pluginID", frame.PluginID, "owner", frame.Owner, "error", err)
	}
}

func (m *Manager) sendUIRequestError(frame Frame, err error) {
	if sendErr := m.send(context.Background(), Frame{
		T:         FrameUIResolved,
		CorrID:    frame.CorrID,
		PluginID:  frame.PluginID,
		Owner:     frame.Owner,
		SessionID: frame.SessionID,
		EnvID:     frame.EnvID,
		IsError:   true,
		Error:     err.Error(),
	}); sendErr != nil {
		m.logger.Error("send plugin ui.request error", "pluginID", frame.PluginID, "owner", frame.Owner, "error", sendErr)
	}
}

func (m *Manager) handleEvent(frame Frame) {
	if m.opts.OnEvent == nil {
		return
	}
	payload := []byte(nil)
	if frame.Payload != nil {
		payload = append(payload, frame.Payload...)
	}
	if err := m.opts.OnEvent(frame.PluginID, frame.Owner, frame.SessionID, frame.Name, payload); err != nil {
		m.logger.Error("plugin event handler failed", "pluginID", frame.PluginID, "owner", frame.Owner, "name", frame.Name, "error", err)
	}
}

func (m *Manager) handleLog(frame Frame) {
	switch frame.Level {
	case "debug":
		m.logger.Debug(frame.Msg, "pluginID", frame.PluginID, "owner", frame.Owner)
	case "info":
		m.logger.Info(frame.Msg, "pluginID", frame.PluginID, "owner", frame.Owner)
	case "warn":
		m.logger.Warn(frame.Msg, "pluginID", frame.PluginID, "owner", frame.Owner)
	case "error":
		m.logger.Error(frame.Msg, "pluginID", frame.PluginID, "owner", frame.Owner)
	default:
		m.logger.Info(frame.Msg, "pluginID", frame.PluginID, "owner", frame.Owner, "level", frame.Level)
	}
}

func (m *Manager) completePending(frame Frame) bool {
	if frame.CorrID == "" {
		return false
	}
	m.mu.Lock()
	resultCh, ok := m.pending[frame.CorrID]
	if ok {
		delete(m.pending, frame.CorrID)
	}
	m.mu.Unlock()
	if ok {
		resultCh <- rpcResult{frame: frame}
	}
	return ok
}

func (m *Manager) removePending(corrID string) {
	m.mu.Lock()
	delete(m.pending, corrID)
	m.mu.Unlock()
}

func (m *Manager) failPendingLocked(err error) {
	for corrID, resultCh := range m.pending {
		delete(m.pending, corrID)
		resultCh <- rpcResult{err: err}
	}
}

func (m *Manager) handleHostDown(gen uint64, cause error) {
	m.mu.Lock()
	if gen != 0 && gen != m.generation {
		m.mu.Unlock()
		return
	}
	if m.down {
		m.mu.Unlock()
		return
	}
	m.down = true
	m.stdin = nil
	m.failPendingLocked(ErrHostDown)
	stopping := m.stopping
	scheduled := m.restartScheduled
	if !stopping && !scheduled {
		m.restartScheduled = true
		m.restartWG.Add(1) // 锁内 Add，与 Stop 设 stopping 串行：Stop 要么先停（不进此支），要么 Wait 必见此 Add
	}
	m.mu.Unlock()

	if cause != nil && !errors.Is(cause, ErrHostDown) {
		m.logger.Warn("plugin host down", "error", cause)
	}
	if !stopping && !scheduled {
		go m.restartLoop()
	}
}

func (m *Manager) restartLoop() {
	defer m.restartWG.Done()
	delay := time.Second
	for {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-m.stopCh:
			timer.Stop()
			return
		}

		if err := m.startProcess(context.Background()); err != nil {
			m.logger.Error("restart plugin host", "error", err, "nextDelay", nextBackoff(delay))
			delay = nextBackoff(delay)
			continue
		}
		m.replayMounts()
		return
	}
}

func nextBackoff(delay time.Duration) time.Duration {
	delay *= 2
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func (m *Manager) replayMounts() {
	for _, entry := range m.mountSnapshot() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		frame, err := m.call(ctx, activateFrame(entry.spec))
		cancel()
		if err != nil {
			m.logger.Error("replay plugin activate", "pluginID", entry.spec.PluginID, "owner", entry.spec.Owner, "error", err)
			continue
		}
		if frame.T != FrameRegistered {
			m.logger.Error("replay plugin activate returned wrong frame", "pluginID", entry.spec.PluginID, "owner", entry.spec.Owner, "type", frame.T)
			continue
		}
		if frame.IsError {
			m.logger.Error("replay plugin activate failed", "pluginID", entry.spec.PluginID, "owner", entry.spec.Owner, "error", frame.Error)
		}
	}
}

func (m *Manager) killGeneration(gen uint64) {
	m.mu.Lock()
	if gen != m.generation || m.cmd == nil || m.cmd.Process == nil || m.down {
		m.mu.Unlock()
		return
	}
	cmd := m.cmd
	m.mu.Unlock()
	_ = cmd.Process.Kill()
}

func (m *Manager) nextCorrID() string {
	return fmt.Sprintf("host-%d", m.corrSeq.Add(1))
}

func activateFrame(spec ActivateSpec) Frame {
	return Frame{
		T:          FrameActivate,
		PluginID:   spec.PluginID,
		Owner:      spec.Owner,
		SessionID:  spec.SessionID,
		EnvID:      spec.EnvID,
		ModulePath: spec.ModulePath,
		CtxSeed:    cloneRaw(spec.CtxSeed),
	}
}

func registeredFromFrame(frame Frame) Registered {
	return Registered{
		Tools:    cloneTools(frame.Tools),
		Hooks:    append([]HookReg(nil), frame.Hooks...),
		Commands: append([]CommandReg(nil), frame.Commands...),
		IsError:  frame.IsError,
		Error:    frame.Error,
	}
}

func cloneTools(in []ToolReg) []ToolReg {
	out := make([]ToolReg, len(in))
	for i, tool := range in {
		out[i] = tool
		out[i].Schema = cloneRaw(tool.Schema)
	}
	return out
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func (m *Manager) recordMount(spec ActivateSpec) {
	spec = cloneSpec(spec)
	key := mountKeyFromSpec(spec)
	id := mountKeyID(key)

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.mounts[id]; ok {
		existing.spec = spec
		existing.key = key
		return
	}
	m.nextSeq++
	m.mounts[id] = &mountEntry{key: key, spec: spec, seq: m.nextSeq}
}

func (m *Manager) removeMount(key MountKey) {
	m.mu.Lock()
	delete(m.mounts, mountKeyID(key))
	m.mu.Unlock()
}

func (m *Manager) mountSnapshot() []mountEntry {
	m.mu.Lock()
	entries := make([]mountEntry, 0, len(m.mounts))
	for _, entry := range m.mounts {
		entries = append(entries, mountEntry{
			key:  entry.key,
			spec: cloneSpec(entry.spec),
			seq:  entry.seq,
		})
	}
	m.mu.Unlock()

	sort.SliceStable(entries, func(i, j int) bool {
		leftRank := ownerRank(entries[i].key.Owner)
		rightRank := ownerRank(entries[j].key.Owner)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return entries[i].seq < entries[j].seq
	})
	return entries
}

func cloneSpec(spec ActivateSpec) ActivateSpec {
	spec.CtxSeed = cloneRaw(spec.CtxSeed)
	return spec
}

func mountKeyFromSpec(spec ActivateSpec) MountKey {
	return MountKey{
		PluginID:  spec.PluginID,
		Owner:     spec.Owner,
		SessionID: spec.SessionID,
		EnvID:     spec.EnvID,
	}
}

func mountKeyID(key MountKey) string {
	return key.PluginID + "\x00" + key.Owner + "\x00" + key.SessionID + "\x00" + key.EnvID
}

func ownerRank(owner string) int {
	switch {
	case owner == "engine":
		return 0
	case owner == "session" || strings.HasPrefix(owner, "session:"):
		return 1
	case owner == "environment" || strings.HasPrefix(owner, "environment:"):
		return 2
	default:
		return 3
	}
}
