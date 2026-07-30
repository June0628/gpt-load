package middleware

import (
	"net/url"
	"strings"
	"testing"
)

func TestRedactSensitiveQuery(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		mustHide   string
		mustRetain string
	}{
		{"auth key param", "key=sk-super-secret&group_id=1", "sk-super-secret", "group_id=1"},
		{"uppercase param", "KEY=sk-super-secret", "sk-super-secret", ""},
		{"token param", "token=abc123&status=active", "abc123", "status=active"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSensitiveQuery(tc.raw)
			if strings.Contains(got, tc.mustHide) {
				t.Errorf("redactSensitiveQuery(%q) = %q, still contains secret", tc.raw, got)
			}
			if tc.mustRetain != "" && !strings.Contains(got, tc.mustRetain) {
				t.Errorf("redactSensitiveQuery(%q) = %q, lost %q", tc.raw, got, tc.mustRetain)
			}
		})
	}
}

func TestRedactSensitiveQueryKeepsUnrelatedQuery(t *testing.T) {
	raw := "group_name=default&page=2"
	if got := redactSensitiveQuery(raw); got != raw {
		t.Errorf("redactSensitiveQuery(%q) = %q, want unchanged", raw, got)
	}
}

func TestRedactSensitiveQueryHandlesInvalidQuery(t *testing.T) {
	raw := "key=%zz"
	if _, err := url.ParseQuery(raw); err == nil {
		t.Fatalf("expected %q to be an invalid query", raw)
	}
	if got := redactSensitiveQuery(raw); got != "[redacted]" {
		t.Errorf("redactSensitiveQuery(%q) = %q, want [redacted]", raw, got)
	}
}
