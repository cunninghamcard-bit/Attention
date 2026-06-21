package protocol

// REST 请求/响应体（along-api.md §2）。增字段只许可选（§3 版本纪律）。
// 错误统一形态：非 2xx + ErrorResponse。
// 无 hello 端点：服务端能力申报走响应 header X-Along-Schema = SchemaVersion。

type CreateSessionRequest struct {
	Agent       string     `json:"agent,omitempty"`       // AgentDef 引用；空 = 默认 native
	Environment string     `json:"environment,omitempty"` // 空 = 进程 cwd 的 local 环境
	CWD         string     `json:"cwd,omitempty"`
	ParentRef   string     `json:"parentRef,omitempty"` // 会话树（D23）
	SpawnedBy   *SpawnedBy `json:"spawnedBy,omitempty"`
}
type SpawnedBy struct {
	SessionID  string `json:"sessionId"`
	RunID      string `json:"runId,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
}
type CreateSessionResponse struct {
	SessionID string `json:"sessionId"`
}

// SessionInfo 同时是 GET /v1/sessions 列表项与 GET /v1/sessions/{id} 响应。
type SessionInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	ParentRef string `json:"parentRef,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
	Modified  string `json:"modified,omitempty"`
}

type ForkSessionRequest struct {
	FromSeq uint64 `json:"fromSeq,omitempty"` // 0 = 最新
}

type PromptRequest struct {
	Text string `json:"text"`
}
type PromptResponse struct {
	RunID string `json:"runId"` // 控制面受理时铸造，随 Job 进队，run.started 复用（对账锚点）
}

type CancelRequest struct {
	LastSeenSeq uint64 `json:"lastSeenSeq,omitempty"` // 对账点（along-api.md §2.1）
}

// 以下三个 P0 不开 REST 端点（along-api.md §6），但它们是 JobQueue 载荷与
// pi RPC 兼容头（Task 13）的过线形态——载荷词汇属于协议，不属于某个头。
type SteerRequest struct {
	Text string `json:"text"`
}
type SetModelRequest struct {
	Model string `json:"model"` // "<provider>/<model-id>"
}
type SetThinkingRequest struct {
	Level string `json:"level"` // off|minimal|low|medium|high|xhigh（与 agentloop.ThinkingLevel 同词汇）
}

type OKResponse struct {
	OK bool `json:"ok"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}
