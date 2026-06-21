package tool

import (
	"sync"
)

// Registry stores tools by name while preserving registration order.
type Registry struct {
	mu    sync.RWMutex
	order []string
	tools map[string]Tool
}

// NewRegistry creates a registry populated with the provided tools.
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{
		order: []string{},
		tools: map[string]Tool{},
	}
	for _, t := range tools {
		r.Add(t)
	}
	return r
}

// Add registers or replaces a tool by name.
func (r *Registry) Add(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[t.Name]; !exists {
		r.order = append(r.order, t.Name)
	}
	r.tools[t.Name] = t
}

// Get returns a registered tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	t, ok := r.tools[name]
	return t, ok
}

// Tools returns registered tools in insertion order.
func (r *Registry) Tools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name])
	}
	return out
}
