package ai

import "encoding/json"

func decodeJSONMap(text string) map[string]any {
	if text == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return map[string]any{}
	}
	return out
}

func usageWithTotals(usage *Usage) *Usage {
	if usage == nil {
		return &Usage{Cost: &Cost{}}
	}
	next := *usage
	next.TotalTokens = next.Input + next.Output + next.CacheRead + next.CacheWrite
	if usage.TotalTokens != 0 {
		next.TotalTokens = usage.TotalTokens
	}
	if usage.Cost != nil {
		cost := *usage.Cost
		next.Cost = &cost
	} else {
		next.Cost = &Cost{}
	}
	return &next
}
