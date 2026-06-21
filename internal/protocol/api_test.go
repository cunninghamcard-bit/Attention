package protocol

import (
	"encoding/json"
	"testing"
)

func TestPromptRequestDecode(t *testing.T) {
	raw := []byte(`{"text":"hi"}`)
	var p PromptRequest
	if err := json.Unmarshal(raw, &p); err != nil || p.Text != "hi" {
		t.Fatalf("bad decode: %v %+v", err, p)
	}
}

func TestErrorBodyShape(t *testing.T) {
	b, _ := json.Marshal(ErrorResponse{Error: ErrorBody{Code: "session_not_found", Message: "no such session"}})
	want := `{"error":{"code":"session_not_found","message":"no such session"}}`
	if string(b) != want {
		t.Fatalf("got %s want %s", b, want)
	}
}
