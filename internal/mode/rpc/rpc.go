// Package rpc implements the JSON-line print-mode subset.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/cunninghamcard-bit/Attention/internal/orchestrator"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

type promptRunner interface {
	Subscribe(func(orchestrator.Event)) func()
	Prompt(context.Context, orchestrator.PromptInput) (orchestrator.PromptResult, error)
	SessionMetadata() session.Metadata
}

// Run sends each prompt sequentially through the orchestrator and writes
// session/event JSON lines.
func Run(ctx context.Context, orch *orchestrator.Orchestrator, prompts []string) error {
	return run(ctx, orch, prompts, os.Stdout)
}

func run(ctx context.Context, orch promptRunner, prompts []string, stdout io.Writer) (err error) {
	writer := newJSONLineWriter(stdout)
	defer func() {
		if flushErr := writer.Flush(); flushErr != nil {
			err = errors.Join(err, flushErr)
		}
	}()

	// pi's json print mode emits the real session header as the first line
	// (print-mode.ts:111-116); the bidirectional rpc server emits none.
	metadata := orch.SessionMetadata()
	header := sessionHeader{
		Type:          "session",
		Version:       3,
		ID:            metadata.ID,
		Timestamp:     metadata.CreatedAt,
		CWD:           metadata.CWD,
		ParentSession: metadata.ParentSessionPath,
	}
	if err := writer.WriteJSON(header); err != nil {
		return err
	}

	cancel := orch.Subscribe(func(ev orchestrator.Event) {
		if value, ok := eventJSONFromOrchestrator(ev); ok {
			_ = writer.WriteJSON(value)
		}
	})
	defer cancel()

	for _, prompt := range prompts {
		if _, promptErr := orch.Prompt(ctx, orchestrator.PromptInput{
			Text:   prompt,
			Source: "rpc",
		}); promptErr != nil {
			return promptErr
		}
	}
	return writer.Err()
}

// sessionHeader mirrors pi's SessionHeader (session-manager.ts:30-37).
type sessionHeader struct {
	Type          string `json:"type"`
	Version       int    `json:"version,omitempty"`
	ID            string `json:"id,omitempty"`
	Timestamp     string `json:"timestamp,omitempty"`
	CWD           string `json:"cwd,omitempty"`
	ParentSession string `json:"parentSession,omitempty"`
}

type typeOnlyEventJSON struct {
	Type string `json:"type"`
}

type agentEndEventJSON struct {
	Type     string       `json:"type"`
	Messages []ai.Message `json:"messages"`
}

type messageEventJSON struct {
	Type    string      `json:"type"`
	Message *ai.Message `json:"message"`
}

type turnEndEventJSON struct {
	Type        string       `json:"type"`
	Message     *ai.Message  `json:"message"`
	ToolResults []ai.Message `json:"toolResults"`
}

type toolExecutionStartEventJSON struct {
	Type       string `json:"type"`
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Args       any    `json:"args"`
}

type toolExecutionUpdateEventJSON struct {
	Type          string `json:"type"`
	ToolCallID    string `json:"toolCallId"`
	ToolName      string `json:"toolName"`
	Args          any    `json:"args"`
	PartialResult any    `json:"partialResult"`
}

type toolExecutionEndEventJSON struct {
	Type       string `json:"type"`
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Result     any    `json:"result"`
	IsError    bool   `json:"isError"`
}

// pi auto retry event shapes:
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:146-147.
type autoRetryStartEventJSON struct {
	Type         string `json:"type"`
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"maxAttempts"`
	DelayMs      int    `json:"delayMs"`
	ErrorMessage string `json:"errorMessage"`
}

type autoRetryEndEventJSON struct {
	Type       string `json:"type"`
	Success    bool   `json:"success"`
	Attempt    int    `json:"attempt"`
	FinalError string `json:"finalError,omitempty"`
}

// pi mode/rpc lifecycle event shapes:
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:122-147.
type thinkingLevelChangedEventJSON struct {
	Type  string `json:"type"`
	Level string `json:"level"`
}

type compactionStartEventJSON struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type compactionEndEventJSON struct {
	Type         string `json:"type"`
	Reason       string `json:"reason"`
	Result       any    `json:"result,omitempty"`
	Aborted      bool   `json:"aborted"`
	WillRetry    bool   `json:"willRetry"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

type compactionResultJSON struct {
	Summary          string `json:"summary"`
	FirstKeptEntryID string `json:"firstKeptEntryId"`
	TokensBefore     int    `json:"tokensBefore"`
	Details          any    `json:"details,omitempty"`
}

type queueUpdateEventJSON struct {
	Type     string   `json:"type"`
	Steering []string `json:"steering"`
	FollowUp []string `json:"followUp"`
}

type savePointEventJSON struct {
	Type                string `json:"type"`
	HadPendingMutations bool   `json:"hadPendingMutations"`
}

type sessionInfoChangedEventJSON struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type settledEventJSON struct {
	Type          string `json:"type"`
	NextTurnCount int    `json:"nextTurnCount"`
}

type resourcesUpdateEventJSON struct {
	Type              string                         `json:"type"`
	Resources         orchestrator.ResourcesSnapshot `json:"resources"`
	PreviousResources orchestrator.ResourcesSnapshot `json:"previousResources"`
}

// message_update + the pi AssistantMessageEvent union it carries
// (.agents/references/pi/packages/agent/src/types.ts:413, 347-359).
type messageUpdateEventJSON struct {
	Type                  string      `json:"type"`
	Message               *ai.Message `json:"message"`
	AssistantMessageEvent any         `json:"assistantMessageEvent"`
}

// blockStartEventJSON: text_start/thinking_start/toolcall_start (types.ts:349,352,355).
type blockStartEventJSON struct {
	Type         string      `json:"type"`
	ContentIndex int         `json:"contentIndex"`
	Partial      *ai.Message `json:"partial"`
}

// blockDeltaEventJSON: text_delta/thinking_delta/toolcall_delta (types.ts:350,353,356).
type blockDeltaEventJSON struct {
	Type         string      `json:"type"`
	ContentIndex int         `json:"contentIndex"`
	Delta        string      `json:"delta"`
	Partial      *ai.Message `json:"partial"`
}

// blockEndEventJSON: text_end/thinking_end/toolcall_end (types.ts:351,354,357).
type blockEndEventJSON struct {
	Type         string      `json:"type"`
	ContentIndex int         `json:"contentIndex"`
	Partial      *ai.Message `json:"partial"`
}

// messageCompleteEventJSON: done/error (types.ts:358-359).
type messageCompleteEventJSON struct {
	Type    string      `json:"type"`
	Message *ai.Message `json:"message"`
}

// mapAssistantMessageEvent maps along's StreamEvent onto the pi
// AssistantMessageEvent union (types.ts:347-359), returning false when along's
// streaming granularity cannot produce a faithful pi event.
//
// Only the variants pi forwards inside message_update are mapped: text/thinking/
// toolcall start + text/thinking delta (pi agent-loop.ts:322-339). pi routes
// done/error to message_end, NOT message_update (agent-loop.ts:341-353); along
// does the same via EventMessageComplete -> MessageEnd, so EventMessageComplete
// is intentionally not mapped here. Also unmapped (along's granularity): per-block
// *_end (no content_block_stop event), toolcall_delta (along carries a decoded
// args map, not pi's incremental JSON string), and image. Tool details reach the
// client via the tool_execution_* events.
func mapAssistantMessageEvent(ev *ai.StreamEvent) (any, bool) {
	if ev == nil || ev.Message == nil {
		return nil, false
	}
	switch ev.Type {
	case ai.EventMessageStart:
		return blockStartEventJSON{Type: "start", Partial: ev.Message}, true
	case ai.EventTextStart:
		return blockStartEventJSON{Type: "text_start", ContentIndex: ev.Index, Partial: ev.Message}, true
	case ai.EventThinkingStart:
		return blockStartEventJSON{Type: "thinking_start", ContentIndex: ev.Index, Partial: ev.Message}, true
	case ai.EventToolCallStart:
		return blockStartEventJSON{Type: "toolcall_start", ContentIndex: ev.Index, Partial: ev.Message}, true
	case ai.EventTextDelta:
		if ev.Delta == nil {
			return nil, false
		}
		return blockDeltaEventJSON{Type: "text_delta", ContentIndex: ev.Index, Delta: ev.Delta.Text, Partial: ev.Message}, true
	case ai.EventThinkingDelta:
		if ev.Delta == nil {
			return nil, false
		}
		return blockDeltaEventJSON{Type: "thinking_delta", ContentIndex: ev.Index, Delta: ev.Delta.Thinking, Partial: ev.Message}, true
	case ai.EventToolCallDelta:
		if ev.Delta == nil {
			return nil, false
		}
		return blockDeltaEventJSON{Type: "toolcall_delta", ContentIndex: ev.Index, Delta: ev.Delta.Text, Partial: ev.Message}, true
	case ai.EventTextEnd:
		return blockEndEventJSON{Type: "text_end", ContentIndex: ev.Index, Partial: ev.Message}, true
	case ai.EventThinkingEnd:
		return blockEndEventJSON{Type: "thinking_end", ContentIndex: ev.Index, Partial: ev.Message}, true
	case ai.EventToolCallEnd:
		return blockEndEventJSON{Type: "toolcall_end", ContentIndex: ev.Index, Partial: ev.Message}, true
	case ai.EventMessageDone:
		return messageCompleteEventJSON{Type: "done", Message: ev.Message}, true
	case ai.EventMessageError:
		return messageCompleteEventJSON{Type: "error", Message: ev.Message}, true
	default:
		return nil, false
	}
}

// eventJSONFromOrchestrator serializes an orchestrator event to its pi
// AgentEvent shape. ok is false when the event maps to no faithful pi event
// (only message_update, when its delta is not mappable) and must not be emitted.
func eventJSONFromOrchestrator(ev orchestrator.Event) (any, bool) {
	// pi AgentEvent union: .agents/references/pi/packages/agent/src/types.ts:403-418.
	// pi rpc-client forwards non-response lines as AgentEvent:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-client.ts:482-497.
	switch ev.Type {
	case orchestrator.EventAgentStart:
		// pi: { type: "agent_start" } (types.ts:405).
		return typeOnlyEventJSON{Type: ev.Type}, true
	case orchestrator.EventAgentEnd:
		// pi: { type: "agent_end"; messages: AgentMessage[] } (types.ts:406).
		return agentEndEventJSON{Type: ev.Type, Messages: nonnilMessages(ev.Messages)}, true
	case orchestrator.EventTurnStart:
		// pi: { type: "turn_start" } (types.ts:408).
		return typeOnlyEventJSON{Type: ev.Type}, true
	case orchestrator.EventTurnEnd:
		// pi: { type: "turn_end"; message: AgentMessage; toolResults: ToolResultMessage[] } (types.ts:409).
		return turnEndEventJSON{
			Type:        ev.Type,
			Message:     ev.Message,
			ToolResults: nonnilMessages(ev.ToolResults),
		}, true
	case orchestrator.EventMessageStart:
		// pi: { type: "message_start"; message: AgentMessage } (types.ts:411).
		return messageEventJSON{Type: ev.Type, Message: ev.Message}, true
	case orchestrator.EventMessageUpdate:
		// pi: { type: "message_update"; message; assistantMessageEvent } (types.ts:413).
		// Skip when the streaming delta has no faithful pi mapping.
		assistantEvent, ok := mapAssistantMessageEvent(ev.Delta)
		if !ok {
			return nil, false
		}
		return messageUpdateEventJSON{
			Type:                  ev.Type,
			Message:               ev.Message,
			AssistantMessageEvent: assistantEvent,
		}, true
	case orchestrator.EventMessageEnd:
		// pi: { type: "message_end"; message: AgentMessage } (types.ts:414).
		return messageEventJSON{Type: ev.Type, Message: ev.Message}, true
	case orchestrator.EventToolExecutionStart:
		// pi: { type: "tool_execution_start"; toolCallId; toolName; args } (types.ts:416).
		return toolExecutionStartEventJSON{
			Type:       ev.Type,
			ToolCallID: ev.ToolCallID,
			ToolName:   ev.ToolName,
			Args:       ev.Args,
		}, true
	case orchestrator.EventToolExecutionUpdate:
		// pi: { type: "tool_execution_update"; toolCallId; toolName; args; partialResult } (types.ts:417).
		return toolExecutionUpdateEventJSON{
			Type:          ev.Type,
			ToolCallID:    ev.ToolCallID,
			ToolName:      ev.ToolName,
			Args:          ev.Args,
			PartialResult: ev.PartialResult,
		}, true
	case orchestrator.EventToolExecutionEnd:
		// pi: { type: "tool_execution_end"; toolCallId; toolName; result; isError } (types.ts:418).
		return toolExecutionEndEventJSON{
			Type:       ev.Type,
			ToolCallID: ev.ToolCallID,
			ToolName:   ev.ToolName,
			Result:     ev.Result,
			IsError:    ev.IsError,
		}, true
	case orchestrator.EventAutoRetryStart:
		return autoRetryStartEventJSON{
			Type:         ev.Type,
			Attempt:      ev.Attempt,
			MaxAttempts:  ev.MaxAttempts,
			DelayMs:      ev.DelayMs,
			ErrorMessage: ev.ErrorMessage,
		}, true
	case orchestrator.EventAutoRetryEnd:
		return autoRetryEndEventJSON{
			Type:       ev.Type,
			Success:    ev.Success,
			Attempt:    ev.Attempt,
			FinalError: ev.FinalError,
		}, true
	case orchestrator.EventThinkingLevelChanged:
		return thinkingLevelChangedEventJSON{
			Type:  ev.Type,
			Level: ev.Level,
		}, true
	case orchestrator.EventCompactionStart:
		return compactionStartEventJSON{
			Type:   ev.Type,
			Reason: ev.Reason,
		}, true
	case orchestrator.EventCompactionEnd:
		return compactionEndEventJSON{
			Type:         ev.Type,
			Reason:       ev.Reason,
			Result:       compactionResultPayload(ev.Result),
			Aborted:      ev.Aborted,
			WillRetry:    ev.WillRetry,
			ErrorMessage: ev.ErrorMessage,
		}, true
	case orchestrator.EventQueueUpdate:
		return queueUpdateEventJSON{
			Type:     ev.Type,
			Steering: nonnilStrings(ev.Steering),
			FollowUp: nonnilStrings(ev.FollowUp),
		}, true
	case orchestrator.EventSavePoint:
		return savePointEventJSON{
			Type:                ev.Type,
			HadPendingMutations: ev.HadPendingMutations,
		}, true
	case orchestrator.EventSessionInfoChanged:
		return sessionInfoChangedEventJSON{
			Type: ev.Type,
			Name: ev.Name,
		}, true
	case orchestrator.EventSettled:
		return settledEventJSON{
			Type:          ev.Type,
			NextTurnCount: ev.NextTurnCount,
		}, true
	case orchestrator.EventResourcesUpdate:
		return resourcesUpdateEventJSON{
			Type:              ev.Type,
			Resources:         ev.Resources,
			PreviousResources: ev.PreviousResources,
		}, true
	default:
		return typeOnlyEventJSON{Type: ev.Type}, true
	}
}

func compactionResultPayload(result any) any {
	switch r := result.(type) {
	case nil:
		return nil
	case orchestrator.CompactResult:
		return compactionResultJSON{
			Summary:          r.Summary,
			FirstKeptEntryID: string(r.FirstKeptEntryID),
			TokensBefore:     r.TokensBefore,
			Details:          r.Details,
		}
	case *orchestrator.CompactResult:
		if r == nil {
			return nil
		}
		return compactionResultJSON{
			Summary:          r.Summary,
			FirstKeptEntryID: string(r.FirstKeptEntryID),
			TokensBefore:     r.TokensBefore,
			Details:          r.Details,
		}
	default:
		return result
	}
}

func nonnilMessages(messages []ai.Message) []ai.Message {
	if messages == nil {
		return []ai.Message{}
	}
	return messages
}

func nonnilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

type jsonLineWriter struct {
	mu  sync.Mutex
	w   *bufio.Writer
	err error
}

func newJSONLineWriter(w io.Writer) *jsonLineWriter {
	return &jsonLineWriter{w: bufio.NewWriter(w)}
}

func (w *jsonLineWriter) WriteJSON(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return w.setErr(err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.err != nil {
		return w.err
	}
	if _, err := w.w.Write(data); err != nil {
		w.err = err
		return err
	}
	if err := w.w.WriteByte('\n'); err != nil {
		w.err = err
		return err
	}
	if err := w.w.Flush(); err != nil {
		w.err = err
		return err
	}
	return nil
}

func (w *jsonLineWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.err != nil {
		return w.err
	}
	if err := w.w.Flush(); err != nil {
		w.err = err
	}
	return w.err
}

func (w *jsonLineWriter) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *jsonLineWriter) setErr(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.err == nil {
		w.err = err
	}
	return w.err
}
