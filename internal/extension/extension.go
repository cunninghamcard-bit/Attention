package extension

import (
	"context"
	"fmt"

	"github.com/cunninghamcard-bit/Attention/internal/hook"
)

// ContextFactory builds a fresh runtime context for an extension callback.
type ContextFactory func(context.Context) ExtensionContext

// Extension represents a loaded extension and the capabilities it registered.
type Extension struct {
	// Path is the source path of the extension (file path or identifier).
	Path string

	// Handlers tracks every (eventType, handler) pair registered via api.On.
	// Keyed by event type for inspection.
	Handlers map[string][]Handler

	// Tools holds all tool definitions registered via api.RegisterTool.
	Tools []ToolDefinition

	// Commands holds all slash-commands registered via api.RegisterCommand.
	Commands map[string]CommandDefinition

	// Providers holds all provider definitions registered via api.RegisterProvider.
	Providers map[string]ProviderDefinition
}

// Load creates an ExtensionAPI, calls the factory, and returns an Extension
// that reflects everything the factory registered.
//
// In this first phase the factory is passed directly. A future phase will
// load factories from shared objects or scripts at the given path.
func Load(
	path string,
	registry *hook.Registry,
	ctxFactory ContextFactory,
	factory Factory,
) (Extension, error) {
	ext := Extension{
		Path:      path,
		Handlers:  make(map[string][]Handler),
		Commands:  make(map[string]CommandDefinition),
		Providers: make(map[string]ProviderDefinition),
	}

	type stagedHandler struct {
		eventType string
		handler   Handler
	}

	var pending []stagedHandler
	var registerErr error
	api := ExtensionAPI{
		On: func(eventType string, handler Handler) {
			ext.Handlers[eventType] = append(ext.Handlers[eventType], handler)
			pending = append(pending, stagedHandler{
				eventType: eventType,
				handler:   handler,
			})
		},
		RegisterTool: func(def ToolDefinition) {
			ext.Tools = append(ext.Tools, def)
		},
		RegisterCommand: func(name string, def CommandDefinition) {
			if _, exists := ext.Commands[name]; exists && registerErr == nil {
				registerErr = fmt.Errorf("extension: command %q already registered", name)
				return
			}
			ext.Commands[name] = def
		},
		RegisterProvider: func(name string, def ProviderDefinition) {
			ext.Providers[name] = def
		},
		UnregisterProvider: func(name string) {
			delete(ext.Providers, name)
		},
	}

	if err := factory(api); err != nil {
		return Extension{}, err
	}
	if registerErr != nil {
		return Extension{}, registerErr
	}

	for _, staged := range pending {
		registry.On(staged.eventType, func(ctx context.Context, event any) (any, error) {
			return staged.handler(ctx, event, ctxFactory(ctx))
		})
	}

	return ext, nil
}
