package worker

// NativeAgent：backend.Agent 的管道实现（D20 native）。
// 装配对照 orchestrator beginRun（internal/orchestrator/orchestrator.go:1529）
// 与 harness createLoopConfig（internal/harness/prompt.go:542）：
// agent 快照 → RunContext → Build(中间件…)(executeRun) → agentloop。

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/pipeline"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

type NativeOptions struct {
	SessionID  string
	Snapshot   func() pipeline.AgentSnapshot // 每 run 拍快照基底（覆写见 SetModel/SetThinking）
	Env        pipeline.EnvView
	Session    pipeline.SessionView
	Emit       pipeline.Emitter
	Stream     agentloop.StreamFunc // 测试注入 fake；生产 = ai provider 流
	Tools      []tool.Tool          // native v1 工具表
	Middleware []pipeline.RunMiddleware
	Hooks      *hook.Registry // nil = no hook callbacks

	// 可选注入（compose 按形态接线；nil = 该能力未接）。
	ResolveModel  func(spec string) (ai.Model, error)         // "<provider>/<model-id>" → ai.Model
	OnModelChange func(ctx context.Context, m ai.Model) error // 持久化（对照 session.AppendModelChange）
}

type NativeAgent struct {
	opts NativeOptions

	mu               sync.Mutex
	cancelRun        context.CancelFunc
	modelOverride    *ai.Model
	thinkingOverride agentloop.ThinkingLevel

	steer chan string
}

func NewNative(opts NativeOptions) *NativeAgent {
	return &NativeAgent{opts: opts, steer: make(chan string, 8)}
}

func (n *NativeAgent) Start(ctx context.Context) error { return nil } // 管道在进程内，无可启动之物

func (n *NativeAgent) HandleInput(ctx context.Context, in backend.Input) error {
	if in.Mode == backend.InputSteer {
		select {
		case n.steer <- in.Text:
		default: // 满则丢弃最旧之外的语义留待 P1；P0 容量 8 足够
		}
		return nil
	}

	tctx, cancel := context.WithCancel(ctx)
	n.mu.Lock()
	n.cancelRun = cancel
	n.mu.Unlock()
	defer func() {
		cancel()
		n.mu.Lock()
		n.cancelRun = nil
		n.mu.Unlock()
	}()

	runID := in.RunID
	if runID == "" {
		runID = protocol.NewRunID() // 同进程路径（print）自铸
	}
	tc := &pipeline.RunContext{
		SessionID: n.opts.SessionID,
		RunID:     runID,
		Agent:     n.snapshot(),
		Env:       n.opts.Env,
		Session:   n.opts.Session,
		Input:     in.Text,
	}
	handler := pipeline.Build(n.executeRun, n.opts.Middleware...)
	err := handler(tctx, tc)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		_ = n.opts.Emit(tc, protocol.KindRunCancelled, protocol.ActorSystem,
			runCancelledPayload{Reason: "cancelled"})
		return nil // 取消是正常收尾，不是错误
	default:
		_ = n.opts.Emit(tc, protocol.KindRunFailed, protocol.ActorSystem,
			runFailedPayload{ErrorClass: "run_error", ErrorMessage: err.Error()})
		return err
	}
}

// executeRun 是链的最内层：把 RunContext 交给 agentloop（存活资产，不动）。
func (n *NativeAgent) executeRun(ctx context.Context, tc *pipeline.RunContext) error {
	sink := pipeline.NewAgentloopSink(n.opts.Emit)
	emit := func(ev agentloop.Event) error {
		sink(ctx, tc, ev)
		return nil
	}

	base := tc.Messages
	if base == nil && tc.Session != nil {
		base = tc.Session.Messages()
	}
	agCtx := agentloop.Context{
		SystemPrompt: tc.Agent.SystemPrompt,
		Messages:     base,
		Tools:        n.opts.Tools,
	}
	cfg := agentloop.Config{
		Model:               tc.Agent.Model,
		ThinkingLevel:       tc.Agent.ThinkingLevel,
		Temperature:         tc.Agent.Temperature,
		MaxTokens:           tc.Agent.MaxTokens,
		SessionID:           tc.SessionID,
		ConvertToLLM:        message.DefaultConvertToLLM,
		GetSteeringMessages: n.drainSteer,
	}
	if n.opts.Hooks != nil {
		callbacks := pipeline.LoopHookCallbacks(n.opts.Hooks)
		cfg.BeforeToolCall = callbacks.BeforeToolCall
		cfg.AfterToolCall = callbacks.AfterToolCall
	}
	prompts := []message.AgentMessage{userMessage(tc.Input)}

	newMessages, err := agentloop.Run(ctx, prompts, agCtx, cfg, n.opts.Stream, emit)
	if err != nil {
		return err
	}
	if tc.Session != nil {
		for _, m := range newMessages {
			if err := tc.Session.AppendMessage(m); err != nil {
				return err
			}
		}
	}
	return nil
}

func (n *NativeAgent) CancelActiveRun(ctx context.Context, reason string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cancelRun != nil {
		n.cancelRun()
	}
	return nil
}

// SetModel/SetThinking：覆写进 runtime 状态（下一 run 经 snapshot 生效，对照
// orchestrator setModel :1395 的 pendingWrites 语义——run 进行中不打断），
// 持久化经 OnModelChange 注入，成功后落 session.changed 信封。
func (n *NativeAgent) SetModel(ctx context.Context, spec string) error {
	if n.opts.ResolveModel == nil {
		return errors.New("worker: model switching not wired (ResolveModel nil)")
	}
	m, err := n.opts.ResolveModel(spec)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.modelOverride = &m
	n.mu.Unlock()
	if n.opts.OnModelChange != nil {
		if err := n.opts.OnModelChange(ctx, m); err != nil {
			return err
		}
	}
	return n.emitSessionChanged(sessionChangedPayload{Model: spec})
}

func (n *NativeAgent) SetThinking(ctx context.Context, level string) error {
	n.mu.Lock()
	n.thinkingOverride = agentloop.ThinkingLevel(level)
	n.mu.Unlock()
	return n.emitSessionChanged(sessionChangedPayload{ThinkingLevel: level})
}

func (n *NativeAgent) Stop(ctx context.Context, reason string) error {
	return n.CancelActiveRun(ctx, reason)
}

func (n *NativeAgent) snapshot() pipeline.AgentSnapshot {
	s := n.opts.Snapshot()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.modelOverride != nil {
		s.Model = *n.modelOverride
	}
	if n.thinkingOverride != "" {
		s.ThinkingLevel = n.thinkingOverride
	}
	return s
}

func (n *NativeAgent) drainSteer(ctx context.Context) ([]message.AgentMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []message.AgentMessage
	for {
		select {
		case t := <-n.steer:
			out = append(out, userMessage(t))
		default:
			return out, nil
		}
	}
}

func (n *NativeAgent) emitSessionChanged(p sessionChangedPayload) error {
	return n.opts.Emit(&pipeline.RunContext{SessionID: n.opts.SessionID},
		protocol.KindSessionChanged, protocol.ActorSystem, p)
}

func userMessage(text string) message.AgentMessage {
	return ai.Message{
		Role:      ai.RoleUser,
		Content:   []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
		Timestamp: time.Now().UnixMilli(),
	}
}

// errorMessage 不用 "message" 键——那是消息事件里 ai.Message 对象的键，
// 同键两型会把折叠端的解码搞炸（与 RetryPayload.ErrorMessage 同词）。
type runFailedPayload struct {
	ErrorClass   string `json:"errorClass"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

type runCancelledPayload struct {
	Reason string `json:"reason,omitempty"`
}

type sessionChangedPayload struct {
	Model         string `json:"model,omitempty"`
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
}
