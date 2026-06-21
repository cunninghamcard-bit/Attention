package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

func ParseModelsConfig(data []byte) (ModelsConfig, error) {
	if strings.TrimSpace(string(data)) == "" {
		return ModelsConfig{Providers: map[string]ProviderConfig{}}, nil
	}

	stripped := []byte(stripJSONComments(string(data)))
	var cfg ModelsConfig
	if err := json.Unmarshal(stripped, &cfg); err != nil {
		return ModelsConfig{}, fmt.Errorf("parse models config: %w", err)
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}
	return cfg, nil
}

func stripJSONComments(input string) string {
	withoutComments := stripJSONLineComments(input)
	return stripJSONTrailingCommas(withoutComments)
}

func stripJSONLineComments(input string) string {
	var out strings.Builder
	out.Grow(len(input))

	var inString bool
	var escaped bool
	for i := 0; i < len(input); i++ {
		c := input[i]
		if inString {
			out.WriteByte(c)
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}

		if c == '"' {
			inString = true
			out.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(input) && input[i+1] == '/' {
			i += 2
			for i < len(input) && input[i] != '\n' {
				i++
			}
			if i < len(input) {
				out.WriteByte(input[i])
			}
			continue
		}
		out.WriteByte(c)
	}

	return out.String()
}

func stripJSONTrailingCommas(input string) string {
	var out strings.Builder
	out.Grow(len(input))

	var inString bool
	var escaped bool
	for i := 0; i < len(input); i++ {
		c := input[i]
		if inString {
			out.WriteByte(c)
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}

		if c == '"' {
			inString = true
			out.WriteByte(c)
			continue
		}
		if c == ',' && commaIsTrailing(input, i) {
			continue
		}
		out.WriteByte(c)
	}

	return out.String()
}

func commaIsTrailing(input string, index int) bool {
	for i := index + 1; i < len(input); i++ {
		if isJSONWhitespace(input[i]) {
			continue
		}
		return input[i] == '}' || input[i] == ']'
	}
	return false
}

func isJSONWhitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
