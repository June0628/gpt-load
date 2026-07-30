package utils

import (
	"bytes"
	"crypto/aes"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func captureLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	original := logrus.StandardLogger().Out
	originalLevel := logrus.GetLevel()
	logrus.SetOutput(&buf)
	logrus.SetLevel(logrus.WarnLevel)
	t.Cleanup(func() {
		logrus.SetOutput(original)
		logrus.SetLevel(originalLevel)
	})
	fn()
	return buf.String()
}

func TestValidatePasswordStrength(t *testing.T) {
	tests := []struct {
		name         string
		password     string
		wantShort    bool
		wantWeakness bool
	}{
		{"strong long password", "Xq7!fLm2Zt9#Wp4vRd8Y", false, false},
		{"short password", "short", true, false},
		{"long but weak", "my-password-is-long-enough", false, true},
		{"short and weak", "admin", true, true},
		{"weak pattern case insensitive", "SuperSecretValueHere", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureLogs(t, func() {
				ValidatePasswordStrength(tt.password, "TEST_FIELD")
			})

			if got := strings.Contains(out, "shorter than 16 characters"); got != tt.wantShort {
				t.Errorf("short warning = %v, want %v (logs: %s)", got, tt.wantShort, out)
			}
			if got := strings.Contains(out, "common weak patterns"); got != tt.wantWeakness {
				t.Errorf("weak pattern warning = %v, want %v (logs: %s)", got, tt.wantWeakness, out)
			}
			if strings.Contains(out, "TEST_FIELD") != (tt.wantShort || tt.wantWeakness) {
				t.Errorf("field name missing from warnings: %s", out)
			}
		})
	}
}

func TestDeriveAESKey(t *testing.T) {
	key := DeriveAESKey("some-password")

	if len(key) != 32 {
		t.Fatalf("DeriveAESKey() length = %d, want 32", len(key))
	}
	if _, err := aes.NewCipher(key); err != nil {
		t.Errorf("derived key rejected by AES: %v", err)
	}
	if !bytes.Equal(key, DeriveAESKey("some-password")) {
		t.Error("DeriveAESKey is not deterministic")
	}
	if bytes.Equal(key, DeriveAESKey("другой-password")) {
		t.Error("different passwords produced the same key")
	}
}
