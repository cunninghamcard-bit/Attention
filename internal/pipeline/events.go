package pipeline

import (
	"context"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

// Emitter 把信封事件交给持久化+广播（worker 注入：Append→EventStore，Publish→NotifyBus）。
type Emitter func(tc *RunContext, kind string, actor protocol.Actor, payload any) error

// AgentloopSink 把 agentloop 生命周期事件翻译为信封。一级翻译，无中间表示。
type AgentloopSink func(ctx context.Context, tc *RunContext, ev agentloop.Event)

func NewAgentloopSink(emit Emitter) AgentloopSink {
	return func(ctx context.Context, tc *RunContext, ev agentloop.Event) {
		kind, actor := translate(ev)
		if kind == "" {
			return
		}
		_ = emit(tc, kind, actor, payloadFor(kind, ev)) // emit 失败由 emitter 侧记日志，不打断 run
	}
}

func translate(ev agentloop.Event) (string, protocol.Actor) {
	switch ev.Type {
	case agentloop.AgentStart:
		return protocol.KindRunStarted, protocol.ActorSystem
	case agentloop.AgentEnd:
		return protocol.KindRunCompleted, protocol.ActorSystem
	case agentloop.TurnStart:
		return protocol.KindTurnStarted, protocol.ActorSystem
	case agentloop.TurnEnd:
		return protocol.KindTurnCompleted, protocol.ActorSystem
	case agentloop.MessageStart:
		return protocol.KindMessageStarted, protocol.ActorAgent
	case agentloop.MessageUpdate:
		// 只有文本/思考增量是日志事实；块开合、toolcall 流式 JSON 等流装饰
		// 不进日志（工具事实走 tool.* 自包含信封；样式是投影，e9）。
		se := ev.AssistantMessageEvent
		if se == nil {
			return "", ""
		}
		switch se.Type {
		case ai.EventThinkingStart:
			return protocol.KindThoughtStarted, protocol.ActorAgent
		case ai.EventThinkingDelta:
			return protocol.KindThoughtDelta, protocol.ActorAgent
		case ai.EventThinkingEnd:
			return protocol.KindThoughtCompleted, protocol.ActorAgent
		case ai.EventTextDelta:
			return protocol.KindMessageDelta, protocol.ActorAgent
		default:
			return "", ""
		}
	case agentloop.MessageEnd:
		return protocol.KindMessageCompleted, protocol.ActorAgent
	case agentloop.ToolExecutionStart:
		// 决定调工具的是 agent（along-api.md §5）；执行回报才是 tool。
		return protocol.KindToolCallStarted, protocol.ActorAgent
	case agentloop.ToolExecutionUpdate:
		return protocol.KindToolUpdate, protocol.ActorTool
	case agentloop.ToolExecutionEnd:
		return protocol.KindToolCompleted, protocol.ActorTool
	default:
		return "", ""
	}
}

// 载荷按 kind 各取所需（along-api.md §5；瘦 payload 裁决 2026-06-12）。
// 完备判据两条（缺字段 = 协议 bug，不许下游补抓）：
//  1. e9 自包含：tool.* 信封凭自身可渲染（args/partialResult/result/isError 全量过线）；
//  2. pirpc fold oracle：信封流必须折得出 pi AgentEvent 流——partial 消息由
//     fold 从 started{role}+delta{text} 累积，completed 携带完整消息（角色/
//     toolCallId/stopReason 等非增量事实只在这里），turn_end/agent_end 由
//     fold 累积。信封只携带事实本身，不携带流装饰。

type toolPayload struct {
	ToolCallID    string `json:"toolCallId"`
	ToolName      string `json:"toolName"`
	Args          any    `json:"args,omitempty"`
	PartialResult any    `json:"partialResult,omitempty"`
	Result        any    `json:"result,omitempty"`
	IsError       bool   `json:"isError,omitempty"`
}

type messageStartedPayload struct {
	Role string `json:"role"`
}

type deltaPayload struct {
	Text         string `json:"text"`
	ContentIndex int    `json:"contentIndex,omitempty"`
}

type messageCompletedPayload struct {
	Message message.AgentMessage `json:"message"`
}

func payloadFor(kind string, ev agentloop.Event) any {
	switch kind {
	case protocol.KindMessageStarted:
		return messageStartedPayload{Role: messageRole(ev.Message)}
	case protocol.KindMessageDelta, protocol.KindThoughtDelta:
		se := ev.AssistantMessageEvent
		text := ""
		if se.Delta != nil {
			if kind == protocol.KindThoughtDelta {
				text = se.Delta.Thinking
			} else {
				text = se.Delta.Text
			}
		}
		return deltaPayload{Text: text, ContentIndex: se.Index}
	case protocol.KindThoughtStarted, protocol.KindThoughtCompleted:
		return deltaPayload{ContentIndex: ev.AssistantMessageEvent.Index}
	case protocol.KindMessageCompleted:
		return messageCompletedPayload{Message: ev.Message}
	case protocol.KindToolCallStarted, protocol.KindToolUpdate, protocol.KindToolCompleted:
		return toolPayload{
			ToolCallID:    ev.ToolCallID,
			ToolName:      ev.ToolName,
			Args:          ev.Args,
			PartialResult: ev.PartialResult,
			Result:        ev.Result,
			IsError:       ev.IsError,
		}
	default:
		return struct{}{} // run.*/turn.*：runId 在信封顶层，无载荷事实
	}
}

func messageRole(m message.AgentMessage) string {
	if aiMsg, ok := message.AsAIMessage(m); ok {
		return string(aiMsg.Role)
	}
	return ""
}
