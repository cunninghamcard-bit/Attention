package ai

import (
	"context"
	"fmt"
	"iter"
)

type Provider interface {
	Stream(ctx context.Context, opts *StreamOptions) iter.Seq2[*StreamEvent, error]
}

var providers = map[API]Provider{
	APIAnthropicMessages:    anthropicProvider{},
	APIOpenAICompletions:    openAICompletionsProvider{},
	APIOpenAIResponses:      openAIResponsesProvider{},
	APIOpenAICodexResponses: codexProvider{},
}

func streamEvents(ctx context.Context, opts StreamOptions) iter.Seq2[*StreamEvent, error] {
	model, ok := modelFromOptions(&opts)
	if !ok {
		return errorIter(fmt.Errorf("unknown model: %s", opts.Model))
	}

	provider, ok := providers[model.API]
	if !ok {
		return errorIter(fmt.Errorf("unsupported API: %s", model.API))
	}

	return provider.Stream(ctx, &opts)
}

func modelFromOptions(opts *StreamOptions) (Model, bool) {
	if opts.ResolvedModel.ID != "" {
		return opts.ResolvedModel, true
	}
	return GetModel("", opts.Model)
}
