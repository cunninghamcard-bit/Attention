package tool_test

import (
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

func TestRegistryToolsReturnsRegisteredTools(t *testing.T) {
	t.Parallel()

	readTool := tool.Tool{Tool: ai.Tool{Name: "read"}}
	writeTool := tool.Tool{Tool: ai.Tool{Name: "write"}}

	registry := tool.NewRegistry(readTool, writeTool)
	got := registry.Tools()

	if len(got) != 2 {
		t.Fatalf("Tools len = %d, want 2", len(got))
	}
	if got[0].Name != "read" || got[1].Name != "write" {
		t.Fatalf("Tools order = [%q %q], want [read write]", got[0].Name, got[1].Name)
	}
}

func TestRegistryGetByName(t *testing.T) {
	t.Parallel()

	registry := tool.NewRegistry(tool.Tool{
		Tool: ai.Tool{Name: "read", Description: "first"},
	})
	registry.Add(tool.Tool{
		Tool: ai.Tool{Name: "read", Description: "replacement"},
	})

	got, ok := registry.Get("read")
	if !ok {
		t.Fatal("Get(read) ok = false, want true")
	}
	if got.Description != "replacement" {
		t.Fatalf("Get(read) description = %q, want replacement", got.Description)
	}

	if _, ok := registry.Get("missing"); ok {
		t.Fatal("Get(missing) ok = true, want false")
	}
}
