package builtin

import (
	"testing"
)

type testArgs struct {
	Name  string `json:"name"           desc:"A name"`
	Count int    `json:"count,omitempty" desc:"A count"`
}

func TestDecodeDeserializesValidArgs(t *testing.T) {
	t.Parallel()
	args := map[string]any{"name": "hello", "count": float64(3)}
	got, err := decode[testArgs](args)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "hello" {
		t.Errorf("Name = %q, want %q", got.Name, "hello")
	}
	if got.Count != 3 {
		t.Errorf("Count = %d, want 3", got.Count)
	}
}

func TestDecodeAcceptsZeroValueForRequiredField(t *testing.T) {
	t.Parallel()
	args := map[string]any{"name": ""}
	got, err := decode[testArgs](args)
	if err != nil {
		t.Fatalf("decode should accept zero-value required field, got: %v", err)
	}
	if got.Name != "" {
		t.Errorf("Name = %q, want empty string", got.Name)
	}
}

func TestDecodeAcceptsMissingRequiredField(t *testing.T) {
	t.Parallel()
	args := map[string]any{}
	got, err := decode[testArgs](args)
	if err != nil {
		t.Fatalf("decode should accept missing required field, got: %v", err)
	}
	if got.Name != "" {
		t.Errorf("Name = %q, want empty string (zero value)", got.Name)
	}
}

func TestDecodeHandlesMissingOptionalField(t *testing.T) {
	t.Parallel()
	args := map[string]any{"name": "hi"}
	got, err := decode[testArgs](args)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Count != 0 {
		t.Errorf("Count = %d, want 0 (zero value for missing optional)", got.Count)
	}
}

func TestSchemaGeneratesCorrectShape(t *testing.T) {
	t.Parallel()
	s := schema[testArgs]()
	if s["type"] != "object" {
		t.Fatalf("type = %v, want object", s["type"])
	}
	props := s["properties"].(map[string]any)
	if _, ok := props["name"]; !ok {
		t.Fatal("missing property 'name'")
	}
	if _, ok := props["count"]; !ok {
		t.Fatal("missing property 'count'")
	}
	req, ok := s["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "name" {
		t.Errorf("required = %v, want [name]", s["required"])
	}
}
