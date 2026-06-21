// Package enginefacade 把引擎（三接口 + repo）适配成 mode 包的 pi 语义面
// （compat 词汇）。同进程，但纪律与远程客户端等同：写路径走 JobQueue（g1），
// 读路径走 EventStore 折叠——没有私有通道（along-api.md §8）。
package enginefacade

import (
	"encoding/json"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/mode/compat"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

// envelopePayload 是信封载荷的解码镜像（pipeline 各 kind 瘦载荷的并集 +
// worker 的 run 收尾/会话变更载荷）。
type envelopePayload struct {
	// tool.*（自包含，e9）
	ToolCallID    string `json:"toolCallId"`
	ToolName      string `json:"toolName"`
	Args          any    `json:"args"`
	PartialResult any    `json:"partialResult"`
	Result        any    `json:"result"`
	IsError       bool   `json:"isError"`

	// message.started{role} / *.delta{text,contentIndex} / message.completed{message}
	Role         string      `json:"role"`
	Text         string      `json:"text"`
	ContentIndex int         `json:"contentIndex"`
	Message      *ai.Message `json:"message"`

	// run 收尾（worker runFailedPayload/runCancelledPayload）
	ErrorClass string `json:"errorClass"`
	Reason     string `json:"reason"`

	// retry（pipeline.RetryPayload）
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"maxAttempts"`
	DelayMs      int    `json:"delayMs"`
	ErrorMessage string `json:"errorMessage"`
	FinalError   string `json:"finalError"`

	// session.changed（worker sessionChangedPayload + 改名）
	Model         string `json:"model"`
	ThinkingLevel string `json:"thinkingLevel"`
	Name          string `json:"name"`
}

// fold 把信封流折叠回 pi 语义事件。这是折叠完备性的活验收：折不出 =
// 协议缺事实，修 protocol 的 kind/payload，不许在这里旁路引擎偷状态。
//
// 持有的全部投影状态（皆可从信封重放重建）：
//   - runMessages：run 内 message.completed 累积 → agent_end.messages
//   - lastAssistant / turnToolResults：turn_end 的 message+toolResults
//     （工具结果本身也是 toolResult 角色的消息，agentloop loop.go:994）
//   - partial：进行中消息（started{role} 开篇、delta{text} 累积）——pi
//     message_update 的 partial 快照由此重建（瘦 payload 裁决 2026-06-12）
type fold struct {
	runMessages     []ai.Message
	lastAssistant   *ai.Message
	turnToolResults []ai.Message
	partial         *ai.Message
}

// appendPartial 把增量文本累进 partial 的对应块（text 或 thinking）。
func (f *fold) appendPartial(role, text string, thinking bool) *ai.Message {
	if f.partial == nil {
		f.partial = &ai.Message{Role: ai.Role(role)}
	}
	blockType := ai.ContentText
	if thinking {
		blockType = ai.ContentThinking
	}
	if n := len(f.partial.Content); n == 0 || f.partial.Content[n-1].Type != blockType {
		f.partial.Content = append(f.partial.Content, ai.ContentBlock{Type: blockType})
	}
	last := &f.partial.Content[len(f.partial.Content)-1]
	if thinking {
		last.Thinking += text
	} else {
		last.Text += text
	}
	return f.partial
}

func (f *fold) apply(env protocol.Envelope) []compat.Event {
	var p envelopePayload
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil // 烂载荷不折；上游写入即校验，这里只防御不修补
		}
	}

	switch env.Kind {
	case protocol.KindRunStarted:
		f.runMessages = nil
		f.lastAssistant = nil
		f.turnToolResults = nil
		return []compat.Event{{Type: compat.EventAgentStart}}

	case protocol.KindTurnStarted:
		f.turnToolResults = nil
		return []compat.Event{{Type: compat.EventTurnStart}}

	case protocol.KindMessageStarted:
		f.partial = &ai.Message{Role: ai.Role(p.Role)}
		return []compat.Event{{Type: compat.EventMessageStart, Message: f.partial}}

	case protocol.KindMessageDelta:
		partial := f.appendPartial(p.Role, p.Text, false)
		delta := &ai.StreamEvent{
			Type:    ai.EventTextDelta,
			Index:   p.ContentIndex,
			Delta:   &ai.ContentBlock{Type: ai.ContentText, Text: p.Text},
			Message: partial,
		}
		return []compat.Event{{Type: compat.EventMessageUpdate, Message: partial, Delta: delta}}

	case protocol.KindThoughtStarted, protocol.KindThoughtDelta, protocol.KindThoughtCompleted:
		seType := ai.EventThinkingDelta
		switch env.Kind {
		case protocol.KindThoughtStarted:
			seType = ai.EventThinkingStart
		case protocol.KindThoughtCompleted:
			seType = ai.EventThinkingEnd
		}
		partial := f.appendPartial(p.Role, p.Text, true)
		delta := &ai.StreamEvent{
			Type:    seType,
			Index:   p.ContentIndex,
			Delta:   &ai.ContentBlock{Type: ai.ContentThinking, Thinking: p.Text},
			Message: partial,
		}
		return []compat.Event{{Type: compat.EventMessageUpdate, Message: partial, Delta: delta}}

	case protocol.KindMessageCompleted:
		f.partial = nil
		if p.Message != nil {
			f.runMessages = append(f.runMessages, *p.Message)
			switch p.Message.Role {
			case ai.RoleAssistant:
				f.lastAssistant = p.Message
				f.turnToolResults = nil
			case ai.RoleToolResult:
				f.turnToolResults = append(f.turnToolResults, *p.Message)
			}
		}
		return []compat.Event{{Type: compat.EventMessageEnd, Message: p.Message}}

	case protocol.KindToolCallStarted:
		return []compat.Event{{
			Type:       compat.EventToolExecutionStart,
			ToolCallID: p.ToolCallID, ToolName: p.ToolName, Args: p.Args,
		}}

	case protocol.KindToolUpdate:
		return []compat.Event{{
			Type:       compat.EventToolExecutionUpdate,
			ToolCallID: p.ToolCallID, ToolName: p.ToolName, Args: p.Args,
			PartialResult: p.PartialResult,
		}}

	case protocol.KindToolCompleted:
		return []compat.Event{{
			Type:       compat.EventToolExecutionEnd,
			ToolCallID: p.ToolCallID, ToolName: p.ToolName,
			Result: p.Result, IsError: p.IsError,
		}}

	case protocol.KindTurnCompleted:
		ev := compat.Event{Type: compat.EventTurnEnd, ToolResults: f.turnToolResults}
		if f.lastAssistant != nil {
			ev.Message = f.lastAssistant
		}
		return []compat.Event{ev}

	case protocol.KindRunCompleted, protocol.KindRunFailed, protocol.KindRunCancelled:
		// pi 的 run 收尾统一是 agent_end{messages}（取消/失败的细节在消息错误里）。
		return []compat.Event{{Type: compat.EventAgentEnd, Messages: nonNilMessages(f.runMessages)}}

	case protocol.KindRetryAttempted:
		return []compat.Event{{
			Type:    compat.EventAutoRetryStart,
			Attempt: p.Attempt, MaxAttempts: p.MaxAttempts,
			DelayMs: p.DelayMs, ErrorMessage: p.ErrorMessage,
		}}

	case protocol.KindRetryExhausted:
		return []compat.Event{{
			Type:    compat.EventAutoRetryEnd,
			Success: false, Attempt: p.Attempt, FinalError: p.FinalError,
		}}

	case protocol.KindCompactionStarted:
		return []compat.Event{{Type: compat.EventCompactionStart, Reason: p.Reason}}

	case protocol.KindCompactionCompleted:
		return []compat.Event{{Type: compat.EventCompactionEnd, Reason: p.Reason, ErrorMessage: p.ErrorMessage}}

	case protocol.KindSessionChanged:
		var out []compat.Event
		if p.ThinkingLevel != "" {
			out = append(out, compat.Event{Type: compat.EventThinkingLevelChanged, Level: p.ThinkingLevel})
		}
		if p.Name != "" {
			out = append(out, compat.Event{Type: compat.EventSessionInfoChanged, Name: p.Name})
		}
		return out

	default:
		return nil // session.created/forked、ext.* 等不属于 pi run 事件面
	}
}

func nonNilMessages(in []ai.Message) []ai.Message {
	if in == nil {
		return []ai.Message{}
	}
	return in
}
