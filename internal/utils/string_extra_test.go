package utils

import (
	"reflect"
	"testing"
)

func TestMaskAPIKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"", ""},
		{"short", "short"},
		{"12345678", "12345678"},
		{"123456789", "1234****6789"},
		{"sk-secret-upstream-key-1234", "sk-s****1234"},
	}

	for _, tt := range tests {
		if got := MaskAPIKey(tt.key); got != tt.want {
			t.Errorf("MaskAPIKey(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestTruncateString(t *testing.T) {
	tests := []struct {
		s         string
		maxLength int
		want      string
	}{
		{"", 5, ""},
		{"hello", 5, "hello"},
		{"hello", 3, "hel"},
		{"héllo", 2, "hé"},
		{"你好世界", 2, "你好"},
		{"hello", 0, ""},
	}

	for _, tt := range tests {
		if got := TruncateString(tt.s, tt.maxLength); got != tt.want {
			t.Errorf("TruncateString(%q, %d) = %q, want %q", tt.s, tt.maxLength, got, tt.want)
		}
	}
}

func TestSplitAndTrim(t *testing.T) {
	tests := []struct {
		name string
		s    string
		sep  string
		want []string
	}{
		{"empty string", "", ",", []string{}},
		{"only separators", ",,,", ",", []string{}},
		{"trims spaces and drops empties", " a , b ,, c ", ",", []string{"a", "b", "c"}},
		{"no separator present", "single", ",", []string{"single"}},
		{"newline separator", "a\n\nb", "\n", []string{"a", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SplitAndTrim(tt.s, tt.sep); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitAndTrim(%q, %q) = %v, want %v", tt.s, tt.sep, got, tt.want)
			}
		})
	}
}

func TestStringToSet(t *testing.T) {
	got := StringToSet(" a , b , a ", ",")

	want := map[string]struct{}{"a": {}, "b": {}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StringToSet() = %v, want %v", got, want)
	}

	if got := StringToSet("", ","); got != nil {
		t.Errorf("StringToSet(\"\") = %v, want nil", got)
	}
	if got := StringToSet(" , ", ","); got != nil {
		t.Errorf("StringToSet(only separators) = %v, want nil", got)
	}
}
