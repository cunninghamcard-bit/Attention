package protocol

import (
	"encoding/json"
	"testing"
)

// JSON 形状钉死（c2/c4 wire 合同；TS 侧同形生成）。
func TestExtCommandRequestShape(t *testing.T) {
	raw := []byte(`{"pluginId":"todo","owner":"session","sessionId":"ses_1","name":"toggle","payload":{"id":3}}`)
	var req ExtCommandRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if req.PluginID != "todo" || req.Owner != "session" || req.Name != "toggle" || string(req.Payload) != `{"id":3}` {
		t.Fatalf("decode: %+v", req)
	}
}

func TestExtCommandXprocPayloadShapes(t *testing.T) {
	payload := ExtCommandJobPayload{
		PluginID:  "todo",
		Owner:     "session",
		SessionID: "ses_1",
		Name:      "setTodos",
		Payload:   json.RawMessage(`{"todos":[]}`),
		CorrID:    "cor_1",
	}
	b, _ := json.Marshal(payload)
	want := `{"pluginId":"todo","owner":"session","sessionId":"ses_1","name":"setTodos","payload":{"todos":[]},"corrId":"cor_1"}`
	if string(b) != want {
		t.Fatalf("ext.command job payload: %s", b)
	}

	result := ExtCommandResultPayload{CorrID: "cor_1", Result: json.RawMessage(`{"ok":true}`)}
	b, _ = json.Marshal(result)
	if string(b) != `{"corrId":"cor_1","result":{"ok":true}}` {
		t.Fatalf("ext.command result payload: %s", b)
	}

	failed := ExtCommandFailedPayload{CorrID: "cor_1", Code: "timeout", Message: "deadline"}
	b, _ = json.Marshal(failed)
	if string(b) != `{"corrId":"cor_1","code":"timeout","message":"deadline"}` {
		t.Fatalf("ext.command failed payload: %s", b)
	}
}

func TestExtCommandResultKindsAreKnownByPrefix(t *testing.T) {
	for _, kind := range []string{KindExtCommandResult, KindExtCommandFailed, "ext.todo.updated"} {
		if !KnownKind(kind) {
			t.Fatalf("KnownKind(%q) = false, want true", kind)
		}
	}
}

func TestUIRoundTripShapes(t *testing.T) {
	reqPayload := UIRequestPayload{
		RequestID: "uir_1", Kind: UIKindConfirm, Title: "sure?", TimeoutMs: 30000,
	}
	b, _ := json.Marshal(reqPayload)
	want := `{"requestId":"uir_1","kind":"confirm","title":"sure?","timeoutMs":30000}`
	if string(b) != want {
		t.Fatalf("ui.request: %s", b)
	}

	resolved := UIResolvedPayload{RequestID: "uir_1", Value: json.RawMessage(`true`), ResolvedBy: "client"}
	b, _ = json.Marshal(resolved)
	want = `{"requestId":"uir_1","value":true,"resolvedBy":"client"}`
	if string(b) != want {
		t.Fatalf("ui.resolved: %s", b)
	}
}
