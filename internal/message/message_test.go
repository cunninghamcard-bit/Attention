package message

import (
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
)

type customMessage struct{}

func (customMessage) IsAgentMessage()       {}
func (customMessage) IsCustomAgentMessage() {}

func TestDefaultConvertToLLMConvertsMessagePointers(t *testing.T) {
	got, err := DefaultConvertToLLM([]AgentMessage{
		ai.Message{Role: ai.RoleUser},
		&ai.Message{Role: ai.RoleAssistant},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("converted messages len = %d, want 2", len(got))
	}
	if got[0].Role != ai.RoleUser {
		t.Fatalf("converted role = %q, want user", got[0].Role)
	}
	if got[1].Role != ai.RoleAssistant {
		t.Fatalf("converted role = %q, want assistant", got[1].Role)
	}
}

func TestDefaultConvertToLLMSkipsUnknownCustomMessages(t *testing.T) {
	got, err := DefaultConvertToLLM([]AgentMessage{
		ai.Message{Role: ai.RoleUser},
		customMessage{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("converted messages len = %d, want 1", len(got))
	}
	if got[0].Role != ai.RoleUser {
		t.Fatalf("converted role = %q, want user", got[0].Role)
	}
}

func TestDefaultConvertToLLMConvertsKnownCustomMessages(t *testing.T) {
	got, err := DefaultConvertToLLM([]AgentMessage{
		CreateCustomMessage("note", "remember this", false, nil, "2026-05-30T00:00:00Z"),
		CreateBranchSummaryMessage("old branch", "entry-1", "2026-05-30T00:00:01Z"),
		CreateCompactionSummaryMessage("earlier context", 123, "2026-05-30T00:00:02Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("converted messages len = %d, want 3", len(got))
	}
	if got[0].Content[0].Text != "remember this" {
		t.Fatalf("custom text = %q, want remember this", got[0].Content[0].Text)
	}
	if got[1].Content[0].Text != BranchSummaryPrefix+"old branch"+BranchSummarySuffix {
		t.Fatalf("branch summary text = %q", got[1].Content[0].Text)
	}
	if got[2].Content[0].Text != CompactionSummaryPrefix+"earlier context"+CompactionSummarySuffix {
		t.Fatalf("compaction summary text = %q", got[2].Content[0].Text)
	}
}

func TestDefaultConvertToLLMSkipsExcludedBashExecution(t *testing.T) {
	got, err := DefaultConvertToLLM([]AgentMessage{
		BashExecutionMessage{
			Command:            "echo hidden",
			Output:             "hidden",
			ExcludeFromContext: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("converted messages len = %d, want 0", len(got))
	}
}

func TestSnapshotConvertsMessagePointerToValue(t *testing.T) {
	got := Snapshot(&ai.Message{Role: ai.RoleUser})
	if _, ok := got.(ai.Message); !ok {
		t.Fatalf("snapshot type = %T, want ai.Message", got)
	}
}
