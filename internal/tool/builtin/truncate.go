package builtin

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	defaultMaxLines     = 2000
	defaultMaxBytes     = 50 * 1024
	grepMaxLineLength   = 500
	bashRollingMaxBytes = defaultMaxBytes * 2
)

type truncationResult struct {
	Content               string `json:"content"`
	Truncated             bool   `json:"truncated"`
	TruncatedBy           string `json:"truncatedBy,omitempty"`
	TotalLines            int    `json:"totalLines"`
	TotalBytes            int    `json:"totalBytes"`
	OutputLines           int    `json:"outputLines"`
	OutputBytes           int    `json:"outputBytes"`
	LastLinePartial       bool   `json:"lastLinePartial"`
	FirstLineExceedsLimit bool   `json:"firstLineExceedsLimit"`
	MaxLines              int    `json:"maxLines"`
	MaxBytes              int    `json:"maxBytes"`
}

type truncationOptions struct {
	maxLines int
	maxBytes int
}

func splitLinesForCounting(content string) []string {
	if content == "" {
		return []string{}
	}

	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func truncateHead(content string, opts truncationOptions) truncationResult {
	maxLines := opts.maxLines
	if maxLines == 0 {
		maxLines = defaultMaxLines
	}
	maxBytes := opts.maxBytes
	if maxBytes == 0 {
		maxBytes = defaultMaxBytes
	}

	totalBytes := len([]byte(content))
	lines := splitLinesForCounting(content)
	totalLines := len(lines)
	if totalLines <= maxLines && totalBytes <= maxBytes {
		return truncationResult{
			Content:     content,
			TotalLines:  totalLines,
			TotalBytes:  totalBytes,
			OutputLines: totalLines,
			OutputBytes: totalBytes,
			MaxLines:    maxLines,
			MaxBytes:    maxBytes,
		}
	}

	if len([]byte(lines[0])) > maxBytes {
		return truncationResult{
			Truncated:             true,
			TruncatedBy:           "bytes",
			TotalLines:            totalLines,
			TotalBytes:            totalBytes,
			FirstLineExceedsLimit: true,
			MaxLines:              maxLines,
			MaxBytes:              maxBytes,
		}
	}

	output := make([]string, 0, min(len(lines), maxLines))
	outputBytes := 0
	truncatedBy := "lines"
	for i, line := range lines {
		if i >= maxLines {
			break
		}
		lineBytes := len([]byte(line))
		if len(output) > 0 {
			lineBytes++
		}
		if outputBytes+lineBytes > maxBytes {
			truncatedBy = "bytes"
			break
		}
		output = append(output, line)
		outputBytes += lineBytes
	}
	if len(output) >= maxLines && outputBytes <= maxBytes {
		truncatedBy = "lines"
	}

	outputContent := strings.Join(output, "\n")
	return truncationResult{
		Content:     outputContent,
		Truncated:   true,
		TruncatedBy: truncatedBy,
		TotalLines:  totalLines,
		TotalBytes:  totalBytes,
		OutputLines: len(output),
		OutputBytes: len([]byte(outputContent)),
		MaxLines:    maxLines,
		MaxBytes:    maxBytes,
	}
}

func truncateTail(content string, opts truncationOptions) truncationResult {
	maxLines := opts.maxLines
	if maxLines == 0 {
		maxLines = defaultMaxLines
	}
	maxBytes := opts.maxBytes
	if maxBytes == 0 {
		maxBytes = defaultMaxBytes
	}

	totalBytes := len([]byte(content))
	lines := splitLinesForCounting(content)
	totalLines := len(lines)
	if totalLines <= maxLines && totalBytes <= maxBytes {
		return truncationResult{
			Content:     content,
			TotalLines:  totalLines,
			TotalBytes:  totalBytes,
			OutputLines: totalLines,
			OutputBytes: totalBytes,
			MaxLines:    maxLines,
			MaxBytes:    maxBytes,
		}
	}

	output := []string{}
	outputBytes := 0
	truncatedBy := "lines"
	lastLinePartial := false
	for i := len(lines) - 1; i >= 0 && len(output) < maxLines; i-- {
		line := lines[i]
		lineBytes := len([]byte(line))
		if len(output) > 0 {
			lineBytes++
		}
		if outputBytes+lineBytes > maxBytes {
			truncatedBy = "bytes"
			if len(output) == 0 {
				line = truncateStringToBytesFromEnd(line, maxBytes)
				output = append([]string{line}, output...)
				outputBytes = len([]byte(line))
				lastLinePartial = true
			}
			break
		}
		output = append([]string{line}, output...)
		outputBytes += lineBytes
	}
	if len(output) >= maxLines && outputBytes <= maxBytes {
		truncatedBy = "lines"
	}

	outputContent := strings.Join(output, "\n")
	return truncationResult{
		Content:         outputContent,
		Truncated:       true,
		TruncatedBy:     truncatedBy,
		TotalLines:      totalLines,
		TotalBytes:      totalBytes,
		OutputLines:     len(output),
		OutputBytes:     len([]byte(outputContent)),
		LastLinePartial: lastLinePartial,
		MaxLines:        maxLines,
		MaxBytes:        maxBytes,
	}
}

func truncateStringToBytesFromEnd(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len([]byte(text)) <= maxBytes {
		return text
	}

	start := len(text)
	used := 0
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:start])
		if r == utf8.RuneError && size == 0 {
			break
		}
		if used+size > maxBytes {
			break
		}
		used += size
		start -= size
	}
	return text[start:]
}

func truncateLine(line string) (string, bool) {
	if len([]rune(line)) <= grepMaxLineLength {
		return line, false
	}
	runes := []rune(line)
	return string(runes[:grepMaxLineLength]) + "... [truncated]", true
}

func formatSize(bytes int) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
}
