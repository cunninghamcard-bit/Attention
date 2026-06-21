package ai

import "regexp"

var retryableErrorPattern = regexp.MustCompile(`(?i)overloaded|provider.?returned.?error|rate.?limit|too many requests|429|500|502|503|504|(?:service|server|internal).?(?:unavailable|error)|network.?(?:error|refused|lost)|connection.?(?:error|refused|lost)|websocket.?(?:closed|error)|other side closed|fetch failed|upstream.?connect|reset before headers|socket hang up|ended without|stream ended before message_stop|http2.*did not get a response|timed? out|timeout|terminated|retry delay`)

func IsRetryableError(msg Message, contextWindow int) bool {
	if msg.StopReason != StopReasonError || msg.ErrorMessage == "" {
		return false
	}
	if IsContextOverflow(msg, contextWindow) {
		return false
	}
	return retryableErrorPattern.MatchString(msg.ErrorMessage)
}
