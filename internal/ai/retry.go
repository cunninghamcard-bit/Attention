package ai

func sdkMaxRetries(value int) (int, bool) {
	switch {
	case value > 0:
		return value, true
	case value < 0:
		return 0, true
	default:
		return 0, false
	}
}
