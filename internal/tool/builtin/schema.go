package builtin

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

type agentArgError struct {
	message string
}

func (e *agentArgError) result() tool.Result {
	return errorResult("%s", e.message)
}

func schema[T any]() map[string]any {
	var zero T
	return schemaFromType(reflect.TypeOf(zero))
}

func decode[T any](args map[string]any) (T, error) {
	var v T
	raw, err := json.Marshal(args)
	if err != nil {
		return v, fmt.Errorf("encode args: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("decode args: %w", err)
	}
	return v, nil
}

func schemaFromType(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	properties := map[string]any{}
	var required []string

	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		jsonTag := f.Tag.Get("json")
		if jsonTag == "-" {
			continue
		}
		name, opts := parseJSONTag(jsonTag)
		if name == "" {
			name = f.Name
		}

		prop := typeToSchema(f.Type)
		if desc := f.Tag.Get("desc"); desc != "" {
			prop["description"] = desc
		}
		properties[name] = prop

		if !opts.contains("omitempty") {
			required = append(required, name)
		}
	}

	s := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func typeToSchema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		items := typeToSchema(t.Elem())
		return map[string]any{"type": "array", "items": items}
	case reflect.Struct:
		return schemaFromType(t)
	case reflect.Map:
		return map[string]any{"type": "object"}
	default:
		return map[string]any{}
	}
}

type tagOpts string

func (o tagOpts) contains(opt string) bool {
	for o != "" {
		var name string
		if idx := strings.Index(string(o), ","); idx >= 0 {
			name, o = string(o)[:idx], o[idx+1:]
		} else {
			name, o = string(o), ""
		}
		if name == opt {
			return true
		}
	}
	return false
}

func parseJSONTag(tag string) (string, tagOpts) {
	if before, after, ok := strings.Cut(tag, ","); ok {
		return before, tagOpts(after)
	}
	return tag, ""
}
