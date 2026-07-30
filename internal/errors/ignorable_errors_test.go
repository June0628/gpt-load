package errors

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsIgnorableError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", errors.New("context canceled"), true},
		{"connection reset by peer", errors.New("read tcp: connection reset by peer"), true},
		{"broken pipe", errors.New("write: broken pipe"), true},
		{"closed network connection", errors.New("use of closed network connection"), true},
		{"request canceled", errors.New("Get \"http://x\": request canceled"), true},
		{"wrapped ignorable", fmt.Errorf("copy body: %w", errors.New("broken pipe")), true},
		{"unrelated error", errors.New("upstream returned 500"), false},
		{"case sensitive match only", errors.New("Context Canceled"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsIgnorableError(tt.err); got != tt.want {
				t.Errorf("IsIgnorableError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsUnCounted(t *testing.T) {
	tests := []struct {
		name     string
		errorMsg string
		want     bool
	}{
		{"empty", "", false},
		{"resource exhausted", "Resource has been exhausted (e.g. check quota)", true},
		{"message too long", "Please reduce the length of the messages", true},
		{"uppercase input still matches", "RESOURCE HAS BEEN EXHAUSTED", true},
		{"counted error", "invalid api key", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUnCounted(tt.errorMsg); got != tt.want {
				t.Errorf("IsUnCounted(%q) = %v, want %v", tt.errorMsg, got, tt.want)
			}
		})
	}
}
