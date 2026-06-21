package orchestrator

import (
	"context"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
)

// Subscribe registers fn for mode-facing lifecycle events.
func (o *Orchestrator) Subscribe(fn func(Event)) (cancel func()) {
	if fn == nil {
		return func() {}
	}

	o.subscribersMu.Lock()
	if o.subscribers == nil {
		o.subscribers = map[uint64]func(Event){}
	}
	id := o.nextSubscriberID
	o.nextSubscriberID++
	o.subscribers[id] = fn
	o.subscribersMu.Unlock()

	return func() {
		o.subscribersMu.Lock()
		delete(o.subscribers, id)
		o.subscribersMu.Unlock()
	}
}

func (o *Orchestrator) registerEventHandlers(registry *hook.Registry) {
	for _, eventType := range []string{
		hook.EventMessageStart,
		hook.EventMessageUpdate,
		hook.EventMessageEnd,
		hook.EventTurnStart,
		hook.EventTurnEnd,
		hook.EventAgentStart,
		hook.EventAgentEnd,
		hook.EventToolExecutionStart,
		hook.EventToolExecutionUpdate,
		hook.EventToolExecutionEnd,
	} {
		registry.On(eventType, func(_ context.Context, event any) (any, error) {
			if ev, ok := modeEventFromHook(event); ok {
				if ev.Type == EventAgentEnd {
					// pi wraps agent_end with willRetry computed from the
					// retry settings and the last assistant message
					// (agent-session.ts:496,542-555).
					ev.WillRetry = o.willRetryAfterAgentEnd(ev.Messages)
				}
				o.publish(ev)
			}
			return nil, nil
		})
	}
}

func (o *Orchestrator) publish(ev Event) {
	o.subscribersMu.Lock()
	if len(o.subscribers) == 0 {
		o.subscribersMu.Unlock()
		return
	}
	subscribers := make([]func(Event), 0, len(o.subscribers))
	for _, fn := range o.subscribers {
		subscribers = append(subscribers, fn)
	}
	o.subscribersMu.Unlock()

	for _, fn := range subscribers {
		fn(ev)
	}
}

func (o *Orchestrator) emitModelSelect(
	ctx context.Context,
	model ai.Model,
	previousModel ai.Model,
	source string,
) {
	if modelsAreEqual(previousModel, model) {
		return
	}
	registry := o.hookRegistry()
	if registry == nil || !registry.HasHandlers(hook.EventModelSelect) {
		return
	}

	// pi guards equal models and emits model_select with set/cycle/restore source:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1400-1409.
	_, _ = registry.Emit(ctx, hook.ModelSelectEvent{
		Type:          hook.EventModelSelect,
		Model:         model,
		PreviousModel: previousModel,
		Source:        source,
	})
}

func (o *Orchestrator) emitThinkingLevelSelect(
	ctx context.Context,
	level agentloop.ThinkingLevel,
	previousLevel agentloop.ThinkingLevel,
) {
	if level == previousLevel {
		return
	}
	registry := o.hookRegistry()
	if registry == nil || !registry.HasHandlers(hook.EventThinkingLevelSelect) {
		return
	}

	// pi emits thinking_level_select alongside thinking_level_changed:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1525-1530.
	_, _ = registry.Emit(ctx, hook.ThinkingLevelSelectEvent{
		Type:          hook.EventThinkingLevelSelect,
		Level:         string(level),
		PreviousLevel: string(previousLevel),
	})
}

func (o *Orchestrator) emitSavePoint(ctx context.Context, hadPendingMutations bool) {
	registry := o.hookRegistry()
	if registry != nil && registry.HasHandlers(hook.EventSavePoint) {
		_, _ = registry.Emit(ctx, hook.SavePointEvent{
			Type:                hook.EventSavePoint,
			HadPendingMutations: hadPendingMutations,
		})
	}

	o.publish(Event{
		Type:                EventSavePoint,
		HadPendingMutations: hadPendingMutations,
	})
}

func (o *Orchestrator) emitSettled(ctx context.Context, nextTurnCount int) {
	registry := o.hookRegistry()
	if registry != nil && registry.HasHandlers(hook.EventSettled) {
		_, _ = registry.Emit(ctx, hook.SettledEvent{
			Type:          hook.EventSettled,
			NextTurnCount: nextTurnCount,
		})
	}

	o.publish(Event{
		Type:          EventSettled,
		NextTurnCount: nextTurnCount,
	})
}

func (o *Orchestrator) emitResourcesUpdate(
	ctx context.Context,
	resources ResourcesSnapshot,
	previous ResourcesSnapshot,
) {
	registry := o.hookRegistry()
	if registry != nil && registry.HasHandlers(hook.EventResourcesUpdate) {
		_, _ = registry.Emit(ctx, hook.ResourcesUpdateEvent{
			Type:              hook.EventResourcesUpdate,
			Resources:         resources,
			PreviousResources: previous,
		})
	}

	o.publish(Event{
		Type:              EventResourcesUpdate,
		Resources:         resources,
		PreviousResources: previous,
	})
}

func modeEventFromHook(event any) (Event, bool) {
	switch e := event.(type) {
	case hook.MessageStartEvent:
		return messageStartEvent(e), true
	case *hook.MessageStartEvent:
		if e == nil {
			return Event{}, false
		}
		return messageStartEvent(*e), true
	case hook.MessageUpdateEvent:
		return messageUpdateEvent(e), true
	case *hook.MessageUpdateEvent:
		if e == nil {
			return Event{}, false
		}
		return messageUpdateEvent(*e), true
	case hook.MessageEndEvent:
		return messageEndEvent(e), true
	case *hook.MessageEndEvent:
		if e == nil {
			return Event{}, false
		}
		return messageEndEvent(*e), true
	case hook.TurnStartEvent:
		return Event{Type: EventTurnStart}, true
	case *hook.TurnStartEvent:
		if e == nil {
			return Event{}, false
		}
		return Event{Type: EventTurnStart}, true
	case hook.TurnEndEvent:
		return turnEndEvent(e), true
	case *hook.TurnEndEvent:
		if e == nil {
			return Event{}, false
		}
		return turnEndEvent(*e), true
	case hook.AgentStartEvent:
		return Event{Type: EventAgentStart}, true
	case *hook.AgentStartEvent:
		if e == nil {
			return Event{}, false
		}
		return Event{Type: EventAgentStart}, true
	case hook.AgentEndEvent:
		return agentEndEvent(e), true
	case *hook.AgentEndEvent:
		if e == nil {
			return Event{}, false
		}
		return agentEndEvent(*e), true
	case hook.ToolExecutionStartEvent:
		return toolExecutionStartEvent(e), true
	case *hook.ToolExecutionStartEvent:
		if e == nil {
			return Event{}, false
		}
		return toolExecutionStartEvent(*e), true
	case hook.ToolExecutionUpdateEvent:
		return toolExecutionUpdateEvent(e), true
	case *hook.ToolExecutionUpdateEvent:
		if e == nil {
			return Event{}, false
		}
		return toolExecutionUpdateEvent(*e), true
	case hook.ToolExecutionEndEvent:
		return toolExecutionEndEvent(e), true
	case *hook.ToolExecutionEndEvent:
		if e == nil {
			return Event{}, false
		}
		return toolExecutionEndEvent(*e), true
	default:
		return Event{}, false
	}
}

func messageStartEvent(event hook.MessageStartEvent) Event {
	return Event{
		Type:    EventMessageStart,
		Message: hookMessage(event.Message),
	}
}

func messageUpdateEvent(event hook.MessageUpdateEvent) Event {
	return Event{
		Type:    EventMessageUpdate,
		Message: hookMessage(event.Message),
		Delta:   hookStreamEvent(event.AssistantMessageEvent),
	}
}

func messageEndEvent(event hook.MessageEndEvent) Event {
	return Event{
		Type:    EventMessageEnd,
		Message: hookMessage(event.Message),
	}
}

func turnEndEvent(event hook.TurnEndEvent) Event {
	return Event{
		Type:        EventTurnEnd,
		Message:     hookMessage(event.Message),
		ToolResults: hookMessages(event.ToolResults),
	}
}

func agentEndEvent(event hook.AgentEndEvent) Event {
	return Event{
		Type:     EventAgentEnd,
		Messages: hookMessages(event.Messages),
	}
}

func toolExecutionStartEvent(event hook.ToolExecutionStartEvent) Event {
	return Event{
		Type:       EventToolExecutionStart,
		ToolCallID: event.ToolCallId,
		ToolName:   event.ToolName,
		Args:       event.Args,
	}
}

func toolExecutionUpdateEvent(event hook.ToolExecutionUpdateEvent) Event {
	return Event{
		Type:          EventToolExecutionUpdate,
		ToolCallID:    event.ToolCallId,
		ToolName:      event.ToolName,
		Args:          event.Args,
		PartialResult: event.PartialResult,
	}
}

func toolExecutionEndEvent(event hook.ToolExecutionEndEvent) Event {
	return Event{
		Type:       EventToolExecutionEnd,
		ToolCallID: event.ToolCallId,
		ToolName:   event.ToolName,
		Result:     event.Result,
		IsError:    event.IsError,
	}
}

func hookMessages(values []any) []ai.Message {
	messages := make([]ai.Message, 0, len(values))
	for _, value := range values {
		msg := hookMessage(value)
		if msg == nil {
			continue
		}
		messages = append(messages, *msg)
	}
	return messages
}

func hookMessage(value any) *ai.Message {
	switch msg := value.(type) {
	case ai.Message:
		return &msg
	case *ai.Message:
		if msg == nil {
			return nil
		}
		copied := *msg
		return &copied
	case message.AgentMessage:
		aiMsg, ok := message.AsAIMessage(msg)
		if !ok {
			return nil
		}
		return &aiMsg
	default:
		return nil
	}
}

func hookStreamEvent(value any) *ai.StreamEvent {
	switch ev := value.(type) {
	case *ai.StreamEvent:
		return ev
	case ai.StreamEvent:
		return &ev
	default:
		return nil
	}
}
