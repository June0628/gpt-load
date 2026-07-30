package errors

import (
	"strings"
	"testing"
)

func TestParseUpstreamError(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"standard openai format", `{"error":{"message":"invalid api key","type":"auth"}}`, "invalid api key"},
		{"standard format trims whitespace", `{"error":{"message":"  spaced  "}}`, "spaced"},
		{"vendor error_msg format", `{"error_msg":"quota exhausted","error_code":17}`, "quota exhausted"},
		{"simple error string format", `{"error":"model not found"}`, "model not found"},
		{"root message format", `{"message":"internal failure"}`, "internal failure"},
		{"non json body returned as is", `<html>502 Bad Gateway</html>`, "<html>502 Bad Gateway</html>"},
		{"empty body", ``, ``},
		{"json without known fields", `{"foo":"bar"}`, `{"foo":"bar"}`},
		{"standard format with empty message falls through", `{"error":{"message":""},"message":"fallback"}`, "fallback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseUpstreamError([]byte(tt.body)); got != tt.want {
				t.Errorf("ParseUpstreamError(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestParseUpstreamErrorTruncatesLongMessage(t *testing.T) {
	long := strings.Repeat("é", maxErrorBodyLength+500)
	got := ParseUpstreamError([]byte(`{"message":"` + long + `"}`))

	if runes := len([]rune(got)); runes != maxErrorBodyLength {
		t.Errorf("got %d runes, want %d", runes, maxErrorBodyLength)
	}
}

func TestTruncateStringCountsRunesNotBytes(t *testing.T) {
	if got := truncateString("héllo", 10); got != "héllo" {
		t.Errorf("short string modified: %q", got)
	}
	if got := truncateString("héllo", 2); got != "hé" {
		t.Errorf("truncateString() = %q, want %q", got, "hé")
	}
}
