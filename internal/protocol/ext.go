package protocol

import "encoding/json"

// 插件通信载荷（plugin-system spec §7 c2/c4）。增字段只许可选（版本纪律）。

// ExtCommandRequest 是插件上行命令（c2）：前端/客户端 → 引擎 → 插件
// commands.on 处理器。owner 决定路由：session 必带 sessionId，
// environment 必带 envId，engine 直达引擎注册表。
type ExtCommandRequest struct {
	PluginID  string          `json:"pluginId"`
	Owner     string          `json:"owner"` // session | engine | environment
	SessionID string          `json:"sessionId,omitempty"`
	EnvID     string          `json:"envId,omitempty"`
	Name      string          `json:"name"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type ExtCommandResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
}

type ExtCommandJobPayload struct {
	PluginID  string          `json:"pluginId"`
	Owner     string          `json:"owner"`
	SessionID string          `json:"sessionId,omitempty"`
	EnvID     string          `json:"envId,omitempty"`
	Name      string          `json:"name"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CorrID    string          `json:"corrId"`
}

type ExtCommandResultPayload struct {
	CorrID string          `json:"corrId"`
	Result json.RawMessage `json:"result,omitempty"`
}

type ExtCommandFailedPayload struct {
	CorrID  string `json:"corrId"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// UI 交互三件套（c4）：问与答都是日志事实，pending 是投影。
// kind v1 四种封闭（confirm/select/input/editor）；ui.notify 无 requestId
// 走 fire-and-forget 事件，不在此结构内。

const (
	UIKindConfirm = "confirm"
	UIKindSelect  = "select"
	UIKindInput   = "input"
	UIKindEditor  = "editor"
)

// UIRequestPayload 是 ui.request 信封载荷（引擎/插件 → 客户端）。
type UIRequestPayload struct {
	RequestID string          `json:"requestId"`
	Kind      string          `json:"kind"`
	Title     string          `json:"title"`
	Body      string          `json:"body,omitempty"`
	Options   []string        `json:"options,omitempty"` // select 用
	Default   json.RawMessage `json:"default,omitempty"`
	TimeoutMs int             `json:"timeoutMs"`
}

// UIResolveRequest 是 ui.resolve 命令体（客户端 → 引擎，first-resolve-wins）。
type UIResolveRequest struct {
	RequestID string          `json:"requestId"`
	Value     json.RawMessage `json:"value"`
}

// UIResolvedPayload 是 ui.resolved 信封载荷（日志闭合：client 应答 /
// 超时取 default / run 取消随之 cancelled）。
type UIResolvedPayload struct {
	RequestID  string          `json:"requestId"`
	Value      json.RawMessage `json:"value,omitempty"`
	ResolvedBy string          `json:"resolvedBy"` // client | timeout | cancelled
}

func NewUIRequestID() string { return newID("uir_") }
