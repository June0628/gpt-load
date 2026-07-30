package encryption

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

const testKey = "a-sufficiently-long-encryption-key"

func TestNewServiceEmptyKeyReturnsNoop(t *testing.T) {
	svc, err := NewService("")
	if err != nil {
		t.Fatalf("NewService() error: %v", err)
	}
	if _, ok := svc.(*noopService); !ok {
		t.Fatalf("NewService(\"\") returned %T, want *noopService", svc)
	}
}

func TestAESRoundTrip(t *testing.T) {
	svc, err := NewService(testKey)
	if err != nil {
		t.Fatalf("NewService() error: %v", err)
	}

	for _, plaintext := range []string{"sk-1234567890", "", "多字节 payload 🎉"} {
		ciphertext, err := svc.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt(%q) error: %v", plaintext, err)
		}
		if plaintext != "" && strings.Contains(ciphertext, plaintext) {
			t.Errorf("ciphertext leaks plaintext: %q", ciphertext)
		}
		got, err := svc.Decrypt(ciphertext)
		if err != nil {
			t.Fatalf("Decrypt() error: %v", err)
		}
		if got != plaintext {
			t.Errorf("round trip = %q, want %q", got, plaintext)
		}
	}
}

func TestAESEncryptUsesRandomNonce(t *testing.T) {
	svc, _ := NewService(testKey)

	first, _ := svc.Encrypt("sk-1234567890")
	second, _ := svc.Encrypt("sk-1234567890")

	if first == second {
		t.Error("Encrypt produced identical ciphertexts for the same plaintext")
	}
}

func TestAESDecryptErrors(t *testing.T) {
	svc, _ := NewService(testKey)

	if got, err := svc.Decrypt(""); err != nil || got != "" {
		t.Errorf("Decrypt(\"\") = %q, %v; want empty string and no error", got, err)
	}

	tests := map[string]string{
		"invalid hex":      "not-hex-data",
		"too short":        "abcd",
		"tampered payload": "",
	}

	valid, _ := svc.Encrypt("sk-1234567890")
	// Flip the last hex nibble to corrupt the GCM tag.
	tampered := valid[:len(valid)-1]
	if valid[len(valid)-1] == '0' {
		tampered += "1"
	} else {
		tampered += "0"
	}
	tests["tampered payload"] = tampered

	for name, ciphertext := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Decrypt(ciphertext); err == nil {
				t.Errorf("Decrypt(%q) expected error, got nil", ciphertext)
			}
		})
	}
}

func TestAESDecryptWithDifferentKeyFails(t *testing.T) {
	svc, _ := NewService(testKey)
	other, _ := NewService("a-totally-different-encryption-key")

	ciphertext, _ := svc.Encrypt("sk-1234567890")

	if _, err := other.Decrypt(ciphertext); err == nil {
		t.Error("decryption with a different key should fail")
	}
}

func TestAESBatchDecrypt(t *testing.T) {
	svc, _ := NewService(testKey)

	first, _ := svc.Encrypt("key-one")
	second, _ := svc.Encrypt("key-two")

	got := svc.BatchDecrypt([]string{first, second, first, "", "bogus"})

	if len(got) != 3 {
		t.Fatalf("BatchDecrypt returned %d entries, want 3 (deduped, empty skipped): %v", len(got), got)
	}
	if got[first] != "key-one" || got[second] != "key-two" {
		t.Errorf("BatchDecrypt = %v", got)
	}
	if got["bogus"] != "failed-to-decrypt" {
		t.Errorf("undecryptable entry = %q, want %q", got["bogus"], "failed-to-decrypt")
	}
	if _, ok := got[""]; ok {
		t.Error("empty ciphertext should be skipped")
	}
}

func TestAESHash(t *testing.T) {
	svc, _ := NewService(testKey)
	other, _ := NewService("a-totally-different-encryption-key")

	if svc.Hash("") != "" {
		t.Error("Hash(\"\") should be empty")
	}
	hash := svc.Hash("sk-1234567890")
	if hash != svc.Hash("sk-1234567890") {
		t.Error("Hash is not deterministic")
	}
	if len(hash) != sha256.Size*2 {
		t.Errorf("hash length = %d, want %d hex chars", len(hash), sha256.Size*2)
	}
	if _, err := hex.DecodeString(hash); err != nil {
		t.Errorf("hash is not valid hex: %v", err)
	}
	if hash == other.Hash("sk-1234567890") {
		t.Error("HMAC hashes should differ between encryption keys")
	}
}

func TestNoopService(t *testing.T) {
	svc, _ := NewService("")

	ciphertext, err := svc.Encrypt("sk-1234567890")
	if err != nil || ciphertext != "sk-1234567890" {
		t.Errorf("noop Encrypt() = %q, %v", ciphertext, err)
	}
	plaintext, err := svc.Decrypt("sk-1234567890")
	if err != nil || plaintext != "sk-1234567890" {
		t.Errorf("noop Decrypt() = %q, %v", plaintext, err)
	}

	got := svc.BatchDecrypt([]string{"a", "b", "a", ""})
	want := map[string]string{"a": "a", "b": "b"}
	if len(got) != len(want) {
		t.Fatalf("noop BatchDecrypt() = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("noop BatchDecrypt()[%q] = %q, want %q", k, got[k], v)
		}
	}

	if svc.Hash("") != "" {
		t.Error("noop Hash(\"\") should be empty")
	}
	sum := sha256.Sum256([]byte("sk-1234567890"))
	if svc.Hash("sk-1234567890") != hex.EncodeToString(sum[:]) {
		t.Error("noop Hash should be plain SHA256")
	}
}
