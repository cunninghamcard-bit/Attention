// Package backend 定义双平面唯一耦合的三接口（D19）与 Agent（D20）。
// 控制面只许 import 本包 + protocol + session；执行面同理。跨平面直接调用 = archguard 打回。
package backend

import (
	"context"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

// EventStore 追加信封并赋会话内单调 seq；desktop=JSONL，server=Postgres（P5）。
type EventStore interface {
	Append(ctx context.Context, e *protocol.Envelope) error // 写前赋 e.Seq
	ReadAfter(ctx context.Context, sessionID string, afterSeq uint64, limit int) ([]protocol.Envelope, error)
}

// NotifyBus 只送"有新东西"空信号；内容一律回 EventStore 取（notify-then-fetch，§7）。
type NotifyBus interface {
	Publish(sessionID string)
	// Subscribe 返回容量 1 的合并信号通道；cancel 幂等。
	Subscribe(sessionID string) (ch <-chan struct{}, cancel func())
}

// JobKind 是执行面收件箱的消息种类（与 client 命令同词汇，cancel-as-message：
// 取消也走队列，不开旁路——Arkloop cancel-as-event 同思路）。
type JobKind string

const (
	JobPrompt            JobKind = "prompt"
	JobSteer             JobKind = "steer"
	JobCancel            JobKind = "cancel"
	JobSetModel          JobKind = "set_model"
	JobSetThinking       JobKind = "set_thinking"
	JobSetSessionName    JobKind = "set_session_name"
	JobPermissionResolve JobKind = "permission_resolve"
	JobStop              JobKind = "stop"
	JobExtCommand        JobKind = "ext_command" // P2：插件命令路由（c2）
	JobUIResolve         JobKind = "ui_resolve"  // P2：ui.resolve → ui.resolved 落日志（c4）
)

type ExtCommandRouteMode string

const (
	ExtCommandRouteInProcess    ExtCommandRouteMode = "in_process"
	ExtCommandRouteCrossProcess ExtCommandRouteMode = "cross_process"
	ExtCommandRouteUnsupported  ExtCommandRouteMode = "unsupported"
)

// ExtCommandWorkerTimeout is the single worker-side timeout source for
// cross-process ext.command dispatch. API handlers only wait for this duration
// plus a small grace window.
const ExtCommandWorkerTimeout = 30 * time.Second

// SessionTargetedJobKinds is the P5.5 owner-worker routing set. Prompt is
// intentionally absent: a prompt lease establishes ownership for its session.
var SessionTargetedJobKinds = []JobKind{
	JobCancel,
	JobSteer,
	JobSetModel,
	JobSetThinking,
	JobStop,
	JobUIResolve,
	JobExtCommand,
	JobSetSessionName,
	JobPermissionResolve,
}

// IsSessionTargetedJobKind reports whether kind must be handled only by the
// worker that owns the session affinity lock.
func IsSessionTargetedJobKind(kind JobKind) bool {
	for _, targeted := range SessionTargetedJobKinds {
		if kind == targeted {
			return true
		}
	}
	return false
}

type Job struct {
	SessionID string
	RunID     string // JobPrompt 必带：控制面受理时铸造（protocol.NewRunID），run.started 复用
	Kind      JobKind
	Payload   []byte // 对应 protocol 请求体（PromptRequest 等）的 JSON
}

type LeasedJob struct {
	Job
	LeaseToken string
	Attempts   int
}

// JobQueue 是控制面→执行面的唯一下行通道；desktop=内存队列，server=jobs 表+lease（P5）。
// desktop local queue 取出即消费: LeaseToken="" 且 Ack/Nack/Heartbeat 均为 no-op。
// server queue 以 lease_token 防串扰；worker 崩溃后由 PgJobQueue reap 到 dead，不 resume。
type JobQueue interface {
	Enqueue(ctx context.Context, m Job) error
	Lease(ctx context.Context) (LeasedJob, error) // 阻塞到有消息或 ctx 结束
	Ack(ctx context.Context, leaseToken string) error
	Nack(ctx context.Context, leaseToken string, retryAfter time.Duration) error
	Heartbeat(ctx context.Context, leaseToken string) error
}

// Agent 是会话执行体合同（D20）——实体名取自协议（session.create{agent}），
// 动作命名取自 mosoo-agent-driver 的 AgentDriverBackend
// （runtimes/agent-driver-backend.ts:140：start/stop/handleInput/
// cancelActiveTurn）——cancelActiveTurn 依 run 命名法转 CancelActiveRun；
// handleMcpExecute（MCP 透传）P4 预留。
// native=管道引擎，外部 agent 适配=翻译器子进程（P4）。
// 注意：没有 Subscribe——事件经 EventStore/NotifyBus 流转，不经 backend 返回（D19）。
type Agent interface {
	Start(ctx context.Context) error // native no-op；P4 agent 适配在此拉起子进程
	HandleInput(ctx context.Context, in Input) error
	CancelActiveRun(ctx context.Context, reason string) error
	SetModel(ctx context.Context, model string) error
	SetThinking(ctx context.Context, level string) error
	Stop(ctx context.Context, reason string) error
}

// Input 是 HandleInput 的统一入参（mosoo handleInput 形）：prompt 与 steer
// 是同一动作的两种投递模式，不是两个方法。
type InputMode string

const (
	InputPrompt InputMode = "prompt"
	InputSteer  InputMode = "steer"
)

type Input struct {
	Mode  InputMode
	Text  string
	RunID string // Mode==InputPrompt 时必带（控制面铸造，沿 Job 传入）
}
