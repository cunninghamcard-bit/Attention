package obs

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// pi: .agents/references/pi/packages/coding-agent/src/core/timings.ts:1-31.

type timingMark struct {
	label string
	ms    int64
}

var startupTimings = struct {
	mu       sync.Mutex
	lastTime time.Time
	marks    []timingMark
}{
	lastTime: time.Now(),
	marks:    []timingMark{},
}

// Enabled reports whether startup timings are enabled.
func Enabled() bool {
	return os.Getenv("ALONG_TIMING") == "1"
}

// Reset clears recorded startup timings and resets the delta clock.
func Reset() {
	if !Enabled() {
		return
	}

	startupTimings.mu.Lock()
	defer startupTimings.mu.Unlock()

	startupTimings.lastTime = time.Now()
	startupTimings.marks = startupTimings.marks[:0]
}

// Time records the elapsed time since the last Time or Reset call.
func Time(label string) {
	if !Enabled() {
		return
	}

	startupTimings.mu.Lock()
	defer startupTimings.mu.Unlock()

	now := time.Now()
	startupTimings.marks = append(startupTimings.marks, timingMark{
		label: label,
		ms:    now.Sub(startupTimings.lastTime).Milliseconds(),
	})
	startupTimings.lastTime = now
}

// Report writes the recorded startup timings.
func Report(w io.Writer) {
	if !Enabled() || w == nil {
		return
	}

	startupTimings.mu.Lock()
	marks := append([]timingMark(nil), startupTimings.marks...)
	startupTimings.mu.Unlock()

	if len(marks) == 0 {
		return
	}

	var total int64
	fmt.Fprintln(w, "\n--- Startup Timings ---")
	for _, mark := range marks {
		total += mark.ms
		fmt.Fprintf(w, "  %s: %dms\n", mark.label, mark.ms)
	}
	fmt.Fprintf(w, "  TOTAL: %dms\n", total)
	fmt.Fprintln(w, "------------------------")
}
