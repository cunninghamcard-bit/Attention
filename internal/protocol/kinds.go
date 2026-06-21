package protocol

import "strings"

// v1 事件种类（spec §4"真实状态迁移全覆盖"）。增量可加，禁删禁改义。
const (
	KindRunStarted   = "run.started"
	KindRunCompleted = "run.completed"
	KindRunFailed    = "run.failed"
	KindRunCancelled = "run.cancelled"

	// turn = run 内一次 provider 调用 + 其工具执行（总纲 R10 两层；pi 折叠需要）。
	KindTurnStarted   = "turn.started"
	KindTurnCompleted = "turn.completed"

	KindMessageStarted   = "message.started"
	KindMessageDelta     = "message.delta"
	KindMessageCompleted = "message.completed"

	KindThoughtStarted   = "thought.started"
	KindThoughtDelta     = "thought.delta"
	KindThoughtCompleted = "thought.completed"

	KindToolCallStarted = "tool.call_started"
	KindToolUpdate      = "tool.update"
	KindToolCompleted   = "tool.completed"

	KindSessionCreated      = "session.created"
	KindSessionChanged      = "session.changed"
	KindSessionForked       = "session.forked"
	KindSessionCapabilities = "session.capabilities"

	KindQueueUpdated = "queue.updated"

	KindCompactionStarted   = "compaction.started"
	KindCompactionCompleted = "compaction.completed"

	KindRetryAttempted = "retry.attempted"
	KindRetryExhausted = "retry.exhausted"

	KindPermissionRequested = "permission.requested"

	KindUIRequest       = "ui.request"
	KindUIResolved      = "ui.resolved" // 答案也是日志事实（plugin-system c4）
	KindUISurfaceOpen   = "ui.surface.open"
	KindUISurfaceUpdate = "ui.surface.update"
	KindUISurfaceClose  = "ui.surface.close"

	KindEnvCreated      = "env.created"
	KindEnvDestroyed    = "env.destroyed"
	KindEnvPreviewReady = "env.preview_ready"

	KindAgentNativeEvent = "agent.native_event"

	KindExtCommandResult = "ext.command.result"
	KindExtCommandFailed = "ext.command.failed"

	ExtKindPrefix = "ext." // ext.<plugin-id>.<name>
)

var knownKinds = map[string]bool{
	KindRunStarted: true, KindRunCompleted: true, KindRunFailed: true, KindRunCancelled: true,
	KindTurnStarted: true, KindTurnCompleted: true,
	KindMessageStarted: true, KindMessageDelta: true, KindMessageCompleted: true,
	KindThoughtStarted: true, KindThoughtDelta: true, KindThoughtCompleted: true,
	KindToolCallStarted: true, KindToolUpdate: true, KindToolCompleted: true,
	KindSessionCreated: true, KindSessionChanged: true, KindSessionForked: true, KindSessionCapabilities: true,
	KindQueueUpdated:      true,
	KindCompactionStarted: true, KindCompactionCompleted: true,
	KindRetryAttempted: true, KindRetryExhausted: true,
	KindPermissionRequested: true,
	KindUIRequest:           true, KindUIResolved: true,
	KindUISurfaceOpen: true, KindUISurfaceUpdate: true, KindUISurfaceClose: true,
	KindEnvCreated: true, KindEnvDestroyed: true, KindEnvPreviewReady: true,
	KindAgentNativeEvent: true,
}

// KnownKind 供测试与调试断言用；运行时消费者必须忽略不认识的 kind（§4），不许据此丢事件。
func KnownKind(k string) bool {
	return knownKinds[k] || strings.HasPrefix(k, ExtKindPrefix)
}
