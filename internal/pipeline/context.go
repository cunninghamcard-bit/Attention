package pipeline

import (
	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
)

// AgentSnapshot 是 run 开始时对 agent-def 拍的不可变快照（§5）。
type AgentSnapshot struct {
	Model         ai.Model
	ThinkingLevel agentloop.ThinkingLevel
	SystemPrompt  string
	Temperature   float64
	MaxTokens     int
}

// EnvView 是环境的只读视图。
type EnvView struct {
	ID           string
	CWD          string
	Capabilities []string
}

// SessionView 是会话存储的 run 内入口（由 worker 注入实现）。
type SessionView interface {
	Messages() []message.AgentMessage           // 当前 leaf 路径上的上下文
	AppendMessage(m message.AgentMessage) error // 持久化新消息
}

// RunContext 只装：三资源视图 + run 态。出现第四类东西 = 设计错误。
type RunContext struct {
	SessionID string
	RunID     string
	Agent     AgentSnapshot
	Env       EnvView
	Session   SessionView

	// run 态（随 run 生灭）
	Input    string
	Messages []message.AgentMessage // mw_context 产出的本 run LLM 上下文
	Aborted  bool
}
