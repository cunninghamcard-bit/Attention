package ai

import "regexp"

// OverflowPatterns detects provider context-overflow errors.
var OverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)prompt is too long`),
	regexp.MustCompile(`(?i)request_too_large`),
	regexp.MustCompile(`(?i)input is too long for requested model`),
	regexp.MustCompile(`(?i)exceeds the context window`),
	regexp.MustCompile(`(?i)exceeds (?:the )?(?:model'?s )?maximum context length of [\d,]+ tokens?`),
	regexp.MustCompile(`(?i)input token count.*exceeds the maximum`),
	regexp.MustCompile(`(?i)maximum prompt length is \d+`),
	regexp.MustCompile(`(?i)reduce the length of the messages`),
	regexp.MustCompile(`(?i)maximum context length is \d+ tokens`),
	regexp.MustCompile(`(?i)input \(\d+ tokens\) is longer than the model'?s context length \(\d+ tokens\)`),
	regexp.MustCompile(`(?i)exceeds the limit of \d+`),
	regexp.MustCompile(`(?i)exceeds the available context size`),
	regexp.MustCompile(`(?i)greater than the context length`),
	regexp.MustCompile(`(?i)context window exceeds limit`),
	regexp.MustCompile(`(?i)exceeded model token limit`),
	regexp.MustCompile(`(?i)too large for model with \d+ maximum context length`),
	regexp.MustCompile(`(?i)model_context_window_exceeded`),
	regexp.MustCompile(`(?i)prompt too long; exceeded (?:max )?context length`),
	regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded`),
	regexp.MustCompile(`(?i)too many tokens`),
	regexp.MustCompile(`(?i)token limit exceeded`),
	regexp.MustCompile(`(?i)^4(?:00|13)\s*(?:status code)?\s*\(no body\)`),
}

// NonOverflowPatterns excludes throttling and rate-limit errors from overflow detection.
var NonOverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(Throttling error|Service unavailable):`),
	regexp.MustCompile(`(?i)rate limit`),
	regexp.MustCompile(`(?i)too many requests`),
}

func IsContextOverflow(msg Message, contextWindow int) bool {
	if msg.StopReason == StopReasonError && msg.ErrorMessage != "" {
		for _, pattern := range NonOverflowPatterns {
			if pattern.MatchString(msg.ErrorMessage) {
				return false
			}
		}
		for _, pattern := range OverflowPatterns {
			if pattern.MatchString(msg.ErrorMessage) {
				return true
			}
		}
	}

	if contextWindow > 0 && msg.StopReason == StopReasonStop && msg.Usage != nil {
		inputTokens := msg.Usage.Input + msg.Usage.CacheRead
		if inputTokens > contextWindow {
			return true
		}
	}

	if contextWindow > 0 && msg.StopReason == StopReasonLength && msg.Usage != nil && msg.Usage.Output == 0 {
		inputTokens := msg.Usage.Input + msg.Usage.CacheRead
		// pi compares against contextWindow * 0.99 in float; integer division
		// truncates the boundary one token early (overflow.ts:143-148).
		if float64(inputTokens) >= float64(contextWindow)*0.99 {
			return true
		}
	}

	return false
}
