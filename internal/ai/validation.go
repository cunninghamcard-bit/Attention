package ai

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// coercePrimitiveByType attempts to coerce a single value to the given JSON
// Schema type. If coercion is impossible the original value is returned.
// Mirrors pi's coercePrimitiveByType (validation.ts:77-149).
func coercePrimitiveByType(value any, typeName string) any {
	switch typeName {
	case "string":
		return coerceToString(value)
	case "number":
		return coerceToNumber(value)
	case "integer":
		return coerceToInteger(value)
	case "boolean":
		return coerceToBoolean(value)
	case "null":
		return coerceToNull(value)
	default:
		return value
	}
}

func coerceToString(value any) any {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		if v == math.Trunc(v) && !math.IsInf(v, 0) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.FormatInt(int64(v), 10)
	case bool:
		return fmt.Sprint(v)
	case nil:
		return ""
	default:
		return value
	}
}

func coerceToNumber(value any) any {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		if v == "" {
			return v
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return v
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return v
		}
		return f
	case bool:
		if v {
			return 1.0
		}
		return 0.0
	case nil:
		return 0.0
	default:
		return value
	}
}

func coerceToInteger(value any) any {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		if v == "" {
			return v
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return v
		}
		if f != math.Trunc(f) || math.IsNaN(f) || math.IsInf(f, 0) {
			return v // "7.5" stays string
		}
		return f
	case bool:
		if v {
			return 1.0
		}
		return 0.0
	case nil:
		return 0.0
	default:
		return value
	}
}

func coerceToBoolean(value any) any {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch v {
		case "true":
			return true
		case "false":
			return false
		default:
			return v
		}
	case float64:
		if v == 1.0 {
			return true
		}
		if v == 0.0 {
			return false
		}
		return v
	case int:
		if v == 1 {
			return true
		}
		if v == 0 {
			return false
		}
		return v
	case nil:
		return false
	default:
		return value
	}
}

func coerceToNull(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
	case float64:
		if v == 0.0 {
			return nil
		}
	case int:
		if v == 0 {
			return nil
		}
	case bool:
		if !v {
			return nil
		}
	}
	return value
}

// matchesJSONType checks whether value already has the named JSON Schema type.
func matchesJSONType(value any, typeName string) bool {
	switch typeName {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		switch v := value.(type) {
		case float64:
			return !math.IsNaN(v) && !math.IsInf(v, 0)
		case int:
			return true
		}
		return false
	case "integer":
		switch v := value.(type) {
		case float64:
			return v == math.Trunc(v) && !math.IsNaN(v) && !math.IsInf(v, 0)
		case int:
			return true
		}
		return false
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	default:
		return false
	}
}

// schemaTypes extracts the "type" field from a JSON Schema as a string slice.
// Handles both "type": "string" and "type": ["string", "number"].
func schemaTypes(schema map[string]any) []string {
	raw, ok := schema["type"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case string:
		return []string{v}
	case []string:
		return append([]string{}, v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, elem := range v {
			if s, ok := elem.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func matchesAnyType(value any, types []string) bool {
	for _, t := range types {
		if matchesJSONType(value, t) {
			return true
		}
	}
	return false
}

// deepCloneValue performs a deep clone via JSON round-trip, mirroring
// structuredClone. Returns the original value on marshal/unmarshal failure.
func deepCloneValue(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var cloned any
	if err := json.Unmarshal(data, &cloned); err != nil {
		return value
	}
	return cloned
}

// subSchemaValidates compiles a JSON sub-schema and validates value against it.
// Returns false on compile or validation error (matching pi's
// getSubSchemaValidator returning undefined on error).
func subSchemaValidates(schema map[string]any, value any) bool {
	sch, err := compileSchema(schema)
	if err != nil {
		return false
	}
	return sch.Validate(value) == nil
}

// coerceWithSchema is the top-level coercion entry point, mirroring pi's
// coerceWithJsonSchema (validation.ts:205-244). All steps run sequentially
// with no early returns — allOf, anyOf, oneOf, type coercion, object, array
// all accumulate into next, matching pi exactly.
func coerceWithSchema(value any, schema map[string]any) any {
	if value == nil {
		types := schemaTypes(schema)
		for _, t := range types {
			coerced := coercePrimitiveByType(nil, t)
			if coerced != nil {
				return coerced
			}
		}
		return nil
	}

	next := value

	// allOf: coerce through each sub-schema sequentially (validation.ts:208-212)
	if allOf, ok := schema["allOf"].([]any); ok {
		for _, sub := range allOf {
			if subSchema, ok := sub.(map[string]any); ok {
				next = coerceWithSchema(next, subSchema)
			}
		}
	}

	// anyOf (validation.ts:214-216)
	if anyOf, ok := schema["anyOf"].([]any); ok {
		next = coerceWithUnionSchema(next, anyOf)
	}

	// oneOf (validation.ts:218-220)
	if oneOf, ok := schema["oneOf"].([]any); ok {
		next = coerceWithUnionSchema(next, oneOf)
	}

	// Primitive type coercion (validation.ts:222-233).
	// Pi checks `schemaTypes.length > 1 && matchesAny` for the skip guard, then
	// uses `candidate !== nextValue` (JS reference equality) to detect change.
	// Go maps/slices panic on !=, so we use a broader guard: skip if value
	// already matches any declared type. This is semantically equivalent because
	// coercePrimitiveByType is a no-op when the value already has the target type.
	types := schemaTypes(schema)
	if len(types) > 0 && !matchesAnyType(next, types) {
		for _, t := range types {
			candidate := coercePrimitiveByType(next, t)
			if matchesJSONType(candidate, t) {
				next = candidate
				break
			}
		}
	}

	// Object coercion (validation.ts:235-237)
	if slices.Contains(types, "object") {
		if obj, ok := next.(map[string]any); ok {
			applyObjectCoercion(obj, schema)
		}
	}

	// Array coercion (validation.ts:239-241)
	if slices.Contains(types, "array") {
		if arr, ok := next.([]any); ok {
			applyArrayCoercion(arr, schema)
		}
	}

	return next
}

// coerceWithUnionSchema tries each sub-schema: deep-clone value, coerce, then
// validate. Returns the first result that passes validation. Mirrors
// validation.ts:193-203.
func coerceWithUnionSchema(value any, schemas []any) any {
	for _, sub := range schemas {
		subSchema, ok := sub.(map[string]any)
		if !ok {
			continue
		}
		cloned := deepCloneValue(value)
		coerced := coerceWithSchema(cloned, subSchema)
		if subSchemaValidates(subSchema, coerced) {
			return coerced
		}
	}
	return value
}

// applyObjectCoercion walks properties and additionalProperties to coerce
// object values in place. Mirrors validation.ts:151-172.
func applyObjectCoercion(obj map[string]any, schema map[string]any) {
	definedKeys := map[string]struct{}{}

	// Walk properties
	if props, ok := schema["properties"].(map[string]any); ok {
		for key, propSchema := range props {
			definedKeys[key] = struct{}{}
			val, exists := obj[key]
			if !exists {
				continue
			}
			if ps, ok := propSchema.(map[string]any); ok {
				obj[key] = coerceWithSchema(val, ps)
			}
		}
	}

	// Walk additionalProperties (only if it's a schema object, not bool)
	if addProps, ok := schema["additionalProperties"].(map[string]any); ok {
		for key, val := range obj {
			if _, defined := definedKeys[key]; defined {
				continue
			}
			obj[key] = coerceWithSchema(val, addProps)
		}
	}
}

// applyArrayCoercion coerces array elements in place. Handles both tuple
// schemas (items is []any) and uniform schemas (items is map[string]any).
// Mirrors validation.ts:174-191.
func applyArrayCoercion(arr []any, schema map[string]any) {
	items, ok := schema["items"]
	if !ok {
		return
	}

	switch v := items.(type) {
	case []any:
		// Tuple: coerce each element with its positional schema.
		// Elements beyond the tuple length are untouched.
		for i, itemSchema := range v {
			if i >= len(arr) {
				break
			}
			if is, ok := itemSchema.(map[string]any); ok {
				arr[i] = coerceWithSchema(arr[i], is)
			}
		}
	case map[string]any:
		// Uniform: coerce all elements with the same schema.
		for i := range arr {
			arr[i] = coerceWithSchema(arr[i], v)
		}
	}
}

// ValidateToolArguments validates tool call arguments against the tool's JSON
// Schema parameters and returns a coerced argument map. This mirrors pi's
// packages/ai/src/utils/validation.ts validateToolArguments.
func ValidateToolArguments(toolName string, params map[string]any, args map[string]any) (map[string]any, error) {
	if len(params) == 0 {
		return args, nil
	}

	coercedValue := coerceWithSchema(deepCloneValue(args), params)
	coerced, ok := coercedValue.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("validation failed for tool %q: coerced arguments are %T, want object", toolName, coercedValue)
	}

	sch, err := compileSchema(params)
	if err != nil {
		return nil, fmt.Errorf("schema compile failed for tool %q: %w", toolName, err)
	}
	if err := sch.Validate(coerced); err != nil {
		return nil, fmt.Errorf("validation failed for tool %q: %w", toolName, err)
	}
	return coerced, nil
}

var schemaCache sync.Map

func compileSchema(params map[string]any) (*jsonschema.Schema, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	key := string(raw)
	if cached, ok := schemaCache.Load(key); ok {
		return cached.(*jsonschema.Schema), nil
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("urn:tool", params); err != nil {
		return nil, err
	}
	sch, err := c.Compile("urn:tool")
	if err != nil {
		return nil, err
	}
	schemaCache.Store(key, sch)
	return sch, nil
}
