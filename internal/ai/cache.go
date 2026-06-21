package ai

import "os"

func normalizeCacheRetention(retention CacheRetention) CacheRetention {
	if retention != "" {
		return retention
	}
	// pi: PI_CACHE_RETENTION=long for backward compatibility, default short
	// (openai-responses.ts:28-39, anthropic.ts:41-52).
	if os.Getenv("PI_CACHE_RETENTION") == "long" {
		return CacheRetentionLong
	}
	return CacheRetentionShort
}

func cacheRetentionEnabled(retention CacheRetention) bool {
	return normalizeCacheRetention(retention) != CacheRetentionNone
}
