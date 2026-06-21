package ai

import (
	"context"
	"iter"
	"time"
)

// AssistantMessageEventStream is a single-consumer stream handle.
// It must be iterated and resolved by one goroutine at a time.
type AssistantMessageEventStream struct {
	events   iter.Seq2[*StreamEvent, error]
	result   *Message
	err      error
	started  bool
	consumed bool
	done     bool
}

func Stream(ctx context.Context, opts StreamOptions) *AssistantMessageEventStream {
	return newStream(streamEvents(ctx, opts))
}

func StreamSimple(
	ctx context.Context,
	model Model,
	llmCtx Context,
	opts SimpleStreamOptions,
) *AssistantMessageEventStream {
	return Stream(ctx, streamOptionsFromSimple(model, llmCtx, opts))
}

func NewAssistantMessageEventStream(
	events iter.Seq2[*StreamEvent, error],
) *AssistantMessageEventStream {
	return newStream(events)
}

func newStream(raw iter.Seq2[*StreamEvent, error]) *AssistantMessageEventStream {
	s := &AssistantMessageEventStream{}
	s.events = func(yield func(*StreamEvent, error) bool) {
		if s.consumed {
			s.err = ErrStreamAlreadyConsumed
			s.done = true
			yield(nil, s.err)
			return
		}
		s.consumed = true

		for event, err := range raw {
			if err != nil {
				s.err = err
				if s.result != nil {
					if s.result.StopReason == "" {
						s.result.StopReason = StopReasonError
					}
					if s.result.ErrorMessage == "" {
						s.result.ErrorMessage = err.Error()
					}
				}
				s.done = true
				yield(nil, err)
				return
			}
			if event != nil && event.Message != nil {
				s.result = event.Message
			}
			if !yield(event, nil) {
				if event != nil && event.Type != EventMessageComplete && event.Type != EventMessageDone && event.Type != EventMessageError && s.result != nil {
					s.result.StopReason = StopReasonAborted
				}
				s.done = true
				return
			}
		}
		s.done = true
	}

	return s
}

func (s *AssistantMessageEventStream) Iter() iter.Seq2[*StreamEvent, error] {
	if s.started {
		panic(ErrStreamAlreadyStarted.Error())
	}
	s.started = true
	return s.events
}

func (s *AssistantMessageEventStream) Result() (*Message, error) {
	if !s.started {
		return nil, ErrStreamNotStarted
	}
	if !s.done {
		return nil, ErrStreamNotDone
	}
	if s.result == nil {
		if s.err != nil {
			return nil, s.err
		}
		return nil, ErrStreamMissingResult
	}
	return s.result, nil
}

func Complete(ctx context.Context, opts StreamOptions) (*Message, error) {
	s := Stream(ctx, opts)
	for _, err := range s.Iter() {
		if err != nil {
			break
		}
	}
	return s.Result()
}

func CompleteSimple(
	ctx context.Context,
	model Model,
	llmCtx Context,
	opts SimpleStreamOptions,
) (*Message, error) {
	return Complete(ctx, streamOptionsFromSimple(model, llmCtx, opts))
}

func newAssistantMessage(model Model) *Message {
	return &Message{
		Role:      RoleAssistant,
		Content:   []ContentBlock{},
		API:       model.API,
		Provider:  model.Provider,
		Model:     model.ID,
		Usage:     &Usage{Cost: &Cost{}},
		Timestamp: time.Now().UnixMilli(),
	}
}

func streamOptionsFromSimple(model Model, llmCtx Context, opts SimpleStreamOptions) StreamOptions {
	return StreamOptions{
		Model:          model.ID,
		ResolvedModel:  model,
		Messages:       llmCtx.Messages,
		SystemPrompt:   llmCtx.SystemPrompt,
		Tools:          llmCtx.Tools,
		Temperature:    opts.Temperature,
		MaxTokens:      opts.MaxTokens,
		APIKey:         opts.APIKey,
		Transport:      opts.Transport,
		CacheRetention: opts.CacheRetention,
		SessionID:      opts.SessionID,
		Headers:        opts.Headers,
		Timeout:        opts.Timeout,
		MaxRetries:     opts.MaxRetries,
		Metadata:       opts.Metadata,
		Reasoning:      opts.Reasoning,
		ThinkingBudgets: copyThinkingBudgets(
			opts.ThinkingBudgets,
		),
		OnPayload:  opts.OnPayload,
		OnResponse: opts.OnResponse,
	}
}

func copyThinkingBudgets(budgets *ThinkingBudgets) *ThinkingBudgets {
	if budgets == nil {
		return nil
	}
	copied := *budgets
	return &copied
}
