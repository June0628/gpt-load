package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"gpt-load/internal/utils"
	"io"
)

// Service defines the encryption interface
type Service interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
	BatchDecrypt(ciphertexts []string) map[string]string
	Hash(plaintext string) string
}

// NewService creates encryption service
func NewService(encryptionKey string) (Service, error) {
	if encryptionKey == "" {
		return &noopService{}, nil
	}

	// Derive AES-256 key from user input and validate strength
	aesKey := utils.DeriveAESKey(encryptionKey)
	utils.ValidatePasswordStrength(encryptionKey, "ENCRYPTION_KEY")

	// Initialize cipher and GCM once for reuse
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	return &aesService{key: aesKey, gcm: gcm}, nil
}

// aesService implements AES-256-GCM encryption
type aesService struct {
	key []byte
	gcm cipher.AEAD
}

func (s *aesService) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := s.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(ciphertext), nil
}

// Decrypt decrypts a single ciphertext string
func (s *aesService) Decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}

	data, err := hex.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("invalid hex data: %w", err)
	}

	nonceSize := s.gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, encrypted := data[:nonceSize], data[nonceSize:]
	plaintext, err := s.gcm.Open(nil, nonce, encrypted, nil)
	if err != nil {
		return "", fmt.Errorf("decryption failed: %w", err)
	}

	return string(plaintext), nil
}

// BatchDecrypt decrypts multiple ciphertexts efficiently
// Returns a map of ciphertext -> plaintext, skipping empty strings and duplicates
func (s *aesService) BatchDecrypt(ciphertexts []string) map[string]string {
	// Deduplicate inputs
	seen := make(map[string]struct{}, len(ciphertexts))
	var uniqueCiphertexts []string
	for _, ct := range ciphertexts {
		if ct == "" {
			continue
		}
		if _, exists := seen[ct]; !exists {
			seen[ct] = struct{}{}
			uniqueCiphertexts = append(uniqueCiphertexts, ct)
		}
	}

	results := make(map[string]string, len(uniqueCiphertexts))
	for _, ct := range uniqueCiphertexts {
		plaintext, err := s.Decrypt(ct)
		if err != nil {
			results[ct] = "failed-to-decrypt"
		} else {
			results[ct] = plaintext
		}
	}

	return results
}

// Hash generates a hash of the plaintext using HMAC-SHA256
func (s *aesService) Hash(plaintext string) string {
	if plaintext == "" {
		return ""
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(plaintext))
	return hex.EncodeToString(mac.Sum(nil))
}

// noopService disables encryption
type noopService struct{}

func (s *noopService) Encrypt(plaintext string) (string, error) {
	return plaintext, nil
}

func (s *noopService) Decrypt(ciphertext string) (string, error) {
	return ciphertext, nil
}

// BatchDecrypt for noopService - returns identity map for unique inputs
func (s *noopService) BatchDecrypt(ciphertexts []string) map[string]string {
	seen := make(map[string]struct{}, len(ciphertexts))
	results := make(map[string]string, len(ciphertexts))
	for _, ct := range ciphertexts {
		if ct == "" {
			continue
		}
		if _, exists := seen[ct]; !exists {
			seen[ct] = struct{}{}
			results[ct] = ct
		}
	}
	return results
}

// Hash generates a hash of the plaintext using SHA256 (no key)
func (s *noopService) Hash(plaintext string) string {
	if plaintext == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(hash[:])
}
