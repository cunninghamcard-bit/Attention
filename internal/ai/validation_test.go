package ai

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestValidateToolArgumentsReturnsErrorForNonObjectCoercedArgs(t *testing.T) {
	_, err := ValidateToolArguments(
		"bad-tool",
		map[string]any{"type": "string"},
		map[string]any{"bad": func() {}},
	)
	if err == nil {
		t.Fatal("ValidateToolArguments error = nil")
	}
	if !strings.Contains(err.Error(), "bad-tool") || !strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("ValidateToolArguments error = %v, want tool name and validation error", err)
	}
}

func TestCoercePrimitiveByType(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		typeName string
		want     any
	}{
		// string → number
		{"string to number: valid float", "3.14", "number", 3.14},
		{"string to number: valid int string", "42", "number", 42.0},
		{"string to number: empty string stays string", "", "number", ""},
		{"string to number: non-numeric stays string", "hello", "number", "hello"},
		{"string to number: NaN stays string", "NaN", "number", "NaN"},
		{"string to number: Infinity stays string", "Infinity", "number", "Infinity"},

		// string → integer
		{"string to integer: valid int", "7", "integer", 7.0},
		{"string to integer: float stays string", "7.5", "integer", "7.5"},
		{"string to integer: non-numeric stays string", "abc", "integer", "abc"},
		{"string to integer: NaN stays string", "NaN", "integer", "NaN"},
		{"bool to integer: true", true, "integer", 1.0},
		{"bool to integer: false", false, "integer", 0.0},

		// string → boolean
		{"string to boolean: true", "true", "boolean", true},
		{"string to boolean: false", "false", "boolean", false},
		{"string to boolean: yes stays string", "yes", "boolean", "yes"},
		{"string to boolean: 1 stays string", "1", "boolean", "1"},

		// number → string
		{"number to string: integer", 42.0, "string", "42"},
		{"number to string: int type", int(5), "string", "5"},
		{"number to string: float", 3.14, "string", "3.14"},

		// bool → string
		{"bool to string: true", true, "string", "true"},
		{"bool to string: false", false, "string", "false"},

		// number → boolean
		{"number to boolean: 1", 1.0, "boolean", true},
		{"number to boolean: 0", 0.0, "boolean", false},
		{"number to boolean: other stays number", 42.0, "boolean", 42.0},

		// bool → number
		{"bool to number: true", true, "number", 1.0},
		{"bool to number: false", false, "number", 0.0},

		// null → number
		{"null to number", nil, "number", 0.0},
		// null → string
		{"null to string", nil, "string", ""},
		// null → boolean
		{"null to boolean", nil, "boolean", false},
		// null → null
		{"null to null", nil, "null", nil},

		// zero-ish → null
		{"empty string to null", "", "null", nil},
		{"zero to null", 0.0, "null", nil},
		{"int zero to null", int(0), "null", nil},
		{"false to null", false, "null", nil},

		// Already correct type: no-op
		{"string already string", "hello", "string", "hello"},
		{"number already number", 3.14, "number", 3.14},
		{"bool already boolean", true, "boolean", true},
		{"int already integer", 42.0, "integer", 42.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coercePrimitiveByType(tt.value, tt.typeName)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("coercePrimitiveByType(%v (%T), %q) = %v (%T), want %v (%T)",
					tt.value, tt.value, tt.typeName, got, got, tt.want, tt.want)
			}
		})
	}
}

func TestMatchesJSONTypeRejectsNonFiniteNumbers(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		typeName string
	}{
		{"number NaN", math.NaN(), "number"},
		{"number infinity", math.Inf(1), "number"},
		{"integer NaN", math.NaN(), "integer"},
		{"integer infinity", math.Inf(1), "integer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if matchesJSONType(tt.value, tt.typeName) {
				t.Fatalf("matchesJSONType(%v, %q) = true, want false", tt.value, tt.typeName)
			}
		})
	}
}

func TestCoerceWithSchemaFlatObject(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"age": map[string]any{"type": "number"},
		},
	}
	value := map[string]any{"age": "25"}
	got := coerceWithSchema(value, schema)
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", got)
	}
	if m["age"] != 25.0 {
		t.Errorf("age = %v (%T), want 25.0", m["age"], m["age"])
	}
}

func TestCoerceWithSchemaNestedObject(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"person": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"age": map[string]any{"type": "number"},
				},
			},
		},
	}
	value := map[string]any{
		"person": map[string]any{"age": "30"},
	}
	got := coerceWithSchema(value, schema)
	m := got.(map[string]any)
	person := m["person"].(map[string]any)
	if person["age"] != 30.0 {
		t.Errorf("person.age = %v (%T), want 30.0", person["age"], person["age"])
	}
}

func TestCoerceWithSchemaUniformArrayItems(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"nums": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "number"},
			},
		},
	}
	value := map[string]any{
		"nums": []any{"1", "2", "3"},
	}
	got := coerceWithSchema(value, schema)
	m := got.(map[string]any)
	arr := m["nums"].([]any)
	want := []any{1.0, 2.0, 3.0}
	if !reflect.DeepEqual(arr, want) {
		t.Errorf("nums = %v, want %v", arr, want)
	}
}

func TestCoerceWithSchemaTupleArrayItems(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pair": map[string]any{
				"type": "array",
				"items": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "number"},
				},
			},
		},
	}
	value := map[string]any{
		"pair": []any{42, "10"},
	}
	got := coerceWithSchema(value, schema)
	m := got.(map[string]any)
	arr := m["pair"].([]any)
	if arr[0] != "42" {
		t.Errorf("pair[0] = %v (%T), want \"42\"", arr[0], arr[0])
	}
	if arr[1] != 10.0 {
		t.Errorf("pair[1] = %v (%T), want 10.0", arr[1], arr[1])
	}
}

func TestCoerceWithSchemaTupleArrayExtraElements(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"data": map[string]any{
				"type": "array",
				"items": []any{
					map[string]any{"type": "number"},
				},
			},
		},
	}
	value := map[string]any{
		"data": []any{"5", "extra"},
	}
	got := coerceWithSchema(value, schema)
	m := got.(map[string]any)
	arr := m["data"].([]any)
	if arr[0] != 5.0 {
		t.Errorf("data[0] = %v (%T), want 5.0", arr[0], arr[0])
	}
	if arr[1] != "extra" {
		t.Errorf("data[1] = %v (%T), want \"extra\" (untouched)", arr[1], arr[1])
	}
}

func TestCoerceWithSchemaAdditionalProperties(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "number"},
	}
	value := map[string]any{
		"x": "10",
		"y": "20",
	}
	got := coerceWithSchema(value, schema)
	m := got.(map[string]any)
	if m["x"] != 10.0 {
		t.Errorf("x = %v (%T), want 10.0", m["x"], m["x"])
	}
	if m["y"] != 20.0 {
		t.Errorf("y = %v (%T), want 20.0", m["y"], m["y"])
	}
}

func TestCoerceWithSchemaAdditionalPropertiesSkipsDefined(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
		"additionalProperties": map[string]any{"type": "number"},
	}
	value := map[string]any{
		"name":  "alice",
		"extra": "99",
	}
	got := coerceWithSchema(value, schema)
	m := got.(map[string]any)
	// "name" uses its property schema (string), stays string
	if m["name"] != "alice" {
		t.Errorf("name = %v (%T), want \"alice\"", m["name"], m["name"])
	}
	// "extra" uses additionalProperties schema (number), coerced
	if m["extra"] != 99.0 {
		t.Errorf("extra = %v (%T), want 99.0", m["extra"], m["extra"])
	}
}

func TestCoerceWithSchemaAllOf(t *testing.T) {
	schema := map[string]any{
		"allOf": []any{
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"age": map[string]any{"type": "number"},
				},
			},
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"active": map[string]any{"type": "boolean"},
				},
			},
		},
	}
	value := map[string]any{
		"age":    "25",
		"active": "true",
	}
	got := coerceWithSchema(value, schema)
	m := got.(map[string]any)
	if m["age"] != 25.0 {
		t.Errorf("age = %v (%T), want 25.0", m["age"], m["age"])
	}
	if m["active"] != true {
		t.Errorf("active = %v (%T), want true", m["active"], m["active"])
	}
}

func TestCoerceWithSchemaAnyOf(t *testing.T) {
	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "number"},
			map[string]any{"type": "string"},
		},
	}
	// "42" can be coerced to number, and number passes validation — first branch wins
	got := coerceWithSchema("42", schema)
	if got != 42.0 {
		t.Errorf("anyOf coercion = %v (%T), want 42.0", got, got)
	}
}

func TestCoerceWithSchemaAnyOfFallback(t *testing.T) {
	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "number"},
			map[string]any{"type": "boolean"},
		},
	}
	// "hello" can't be coerced to number or boolean → return original
	got := coerceWithSchema("hello", schema)
	if got != "hello" {
		t.Errorf("anyOf fallback = %v (%T), want \"hello\"", got, got)
	}
}

func TestCoerceWithSchemaOneOf(t *testing.T) {
	schema := map[string]any{
		"oneOf": []any{
			map[string]any{"type": "number"},
			map[string]any{"type": "string"},
		},
	}
	got := coerceWithSchema("42", schema)
	if got != 42.0 {
		t.Errorf("oneOf coercion = %v (%T), want 42.0", got, got)
	}
}

func TestCoerceWithSchemaUnknownPropertiesUntouched(t *testing.T) {
	schema := map[string]any{
		"type": "object",
	}
	value := map[string]any{
		"foo": "bar",
		"num": 42.0,
	}
	got := coerceWithSchema(value, schema)
	m := got.(map[string]any)
	if m["foo"] != "bar" {
		t.Errorf("foo = %v, want \"bar\"", m["foo"])
	}
	if m["num"] != 42.0 {
		t.Errorf("num = %v, want 42.0", m["num"])
	}
}

func TestCoerceWithSchemaNilReturnsNil(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
	}
	got := coerceWithSchema(nil, schema)
	if got != nil {
		t.Errorf("nil coercion = %v (%T), want nil", got, got)
	}
}

func TestCoerceWithSchemaNoProperties(t *testing.T) {
	schema := map[string]any{
		"type": "object",
	}
	value := map[string]any{"x": "1"}
	got := coerceWithSchema(value, schema)
	m := got.(map[string]any)
	if m["x"] != "1" {
		t.Errorf("x = %v, want \"1\" (untouched)", m["x"])
	}
}

func TestCoerceWithSchemaMultiTypeUnion(t *testing.T) {
	// value already matches one of the types — no coercion needed
	schema := map[string]any{
		"type": []any{"string", "number"},
	}
	got := coerceWithSchema("hello", schema)
	if got != "hello" {
		t.Errorf("multi-type union = %v (%T), want \"hello\"", got, got)
	}
}

func TestCoerceWithSchemaMultiTypeUnionCoerces(t *testing.T) {
	// value matches neither type; coerce to first viable
	schema := map[string]any{
		"type": []any{"number", "boolean"},
	}
	got := coerceWithSchema("42", schema)
	if got != 42.0 {
		t.Errorf("multi-type union coercion = %v (%T), want 42.0", got, got)
	}
}

func TestCoerceWithSchemaMultiTypeUnionStringSlice(t *testing.T) {
	schema := map[string]any{
		"type": []string{"number", "boolean"},
	}
	got := coerceWithSchema("42", schema)
	if got != 42.0 {
		t.Errorf("multi-type []string union coercion = %v (%T), want 42.0", got, got)
	}
}

func TestCoerceWithSchemaAllOfPlusProperties(t *testing.T) {
	// pi runs allOf then continues to properties — no early return.
	// A schema with both allOf and top-level properties must coerce both.
	schema := map[string]any{
		"type": "object",
		"allOf": []any{
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "number"},
				},
			},
		},
		"properties": map[string]any{
			"b": map[string]any{"type": "boolean"},
		},
	}
	args := map[string]any{"a": "1", "b": "true"}
	got := coerceWithSchema(args, schema).(map[string]any)
	if got["a"] != float64(1) {
		t.Errorf("a = %v (%T), want float64(1)", got["a"], got["a"])
	}
	if got["b"] != true {
		t.Errorf("b = %v (%T), want true — allOf must not short-circuit properties", got["b"], got["b"])
	}
}
