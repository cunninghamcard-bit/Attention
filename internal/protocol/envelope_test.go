package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	e := Envelope{
		ID: NewEventID(), SessionID: "ses_1", Seq: 42,
		Kind: KindMessageDelta, Actor: ActorAgent,
		Payload:       json.RawMessage(`{"text":"hi"}`),
		OccurredAt:    time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
		SchemaVersion: SchemaVersion,
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"sessionId":"ses_1"`, `"seq":42`, `"kind":"message.delta"`, `"schemaVersion":"1"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s in %s", want, b)
		}
	}
	var back Envelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Seq != 42 || back.Kind != KindMessageDelta {
		t.Fatalf("roundtrip mismatch: %+v", back)
	}
}

func TestKnownKind(t *testing.T) {
	for _, k := range []string{KindRunStarted, KindToolCallStarted, "ext.my-plugin.foo", KindAgentNativeEvent} {
		if !KnownKind(k) {
			t.Fatalf("%s should be known", k)
		}
	}
	if KnownKind("bogus.kind") {
		t.Fatal("bogus.kind should be unknown")
	}
}
