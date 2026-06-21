package sseparse

import (
	"strings"
	"testing"
)

func TestParseSSE(t *testing.T) {
	input := strings.NewReader(": ignored\nid: 7\nevent: message\ndata: {\"a\":1}\ndata: {\"b\":2}\nretry: 1000\n\n")

	var events []Event
	for event, err := range Parse(input) {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got := events[0]
	if got.ID != "7" || got.Event != "message" || got.Retry != "1000" {
		t.Fatalf("metadata = %+v", got)
	}
	if got.Data != "{\"a\":1}\n{\"b\":2}" {
		t.Fatalf("data = %q", got.Data)
	}
}

func TestParseSSEFlushesFinalEventWithoutBlankLine(t *testing.T) {
	input := strings.NewReader("event: done\ndata: [DONE]\n")

	var got []Event
	for event, err := range Parse(input) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, event)
	}

	if len(got) != 1 || got[0].Event != "done" || got[0].Data != "[DONE]" {
		t.Fatalf("got %+v", got)
	}
}
