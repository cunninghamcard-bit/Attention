package extension

import "errors"

// ErrNoInteractiveUI is returned by NoopUIContext for UI calls that need input.
var ErrNoInteractiveUI = errors.New("extension: no interactive UI available")

// NoopUIContext is the non-interactive fallback UI context.
type NoopUIContext struct{}

func (NoopUIContext) Select(string, []string) (int, error) {
	return -1, ErrNoInteractiveUI
}

func (NoopUIContext) Confirm(string) (bool, error) {
	return false, ErrNoInteractiveUI
}

func (NoopUIContext) Input(string) (string, error) {
	return "", ErrNoInteractiveUI
}

func (NoopUIContext) Editor(string, string) (string, error) {
	return "", ErrNoInteractiveUI
}

func (NoopUIContext) Notify(string) {}

func (NoopUIContext) SetStatus(string, string) {}

func (NoopUIContext) SetWidget(string, []string) {}

func (NoopUIContext) SetTitle(string) {}

func (NoopUIContext) SetEditorText(string) {}
