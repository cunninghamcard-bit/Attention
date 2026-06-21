package obs

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestTimingsDisabledNoop(t *testing.T) {
	unsetEnv(t, "ALONG_TIMING")

	Time("disabled mark")

	var out bytes.Buffer
	Report(&out)
	if out.Len() != 0 {
		t.Fatalf("Report wrote %q, want empty when disabled", out.String())
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()

	oldValue, hadValue := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if hadValue {
			_ = os.Setenv(key, oldValue)
			return
		}
		_ = os.Unsetenv(key)
	})
}

func TestTimingsEnabledRecordsLabels(t *testing.T) {
	t.Setenv("ALONG_TIMING", "1")

	Reset()
	Time("config load")
	Time("provider build")

	var out bytes.Buffer
	Report(&out)
	got := out.String()
	for _, want := range []string{"config load:", "provider build:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Report output = %q, want label %q", got, want)
		}
	}
}
