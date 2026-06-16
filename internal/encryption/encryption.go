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

// Service 定义加密接口
type Service interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
	BatchDecrypt(ciphertexts []string) map[string]string
	Hash(plaintext string) string
}

// NewService 创建加密服务
func NewService(encryptionKey string) (Service, error) {
	if encryptionKey == "" {
		return &noopService{}, nil
	}

	// 从用户输入派生 AES-256 密钥并验证强度
	aesKey := utils.DeriveAESKey(encryptionKey)
	utils.ValidatePasswordStrength(encryptionKey, "ENCRYPTION_KEY")

	// 初始化 cipher 和 GCM 以供复用
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

// aesService 实现 AES-256-GCM 加密
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

// Decrypt 解密单个密文字符串
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

// BatchDecrypt 批量解密多个密文
// 返回密文到明文的映射，跳过空字符串和重复项
func (s *aesService) BatchDecrypt(ciphertexts []string) map[string]string {
	// 去重输入
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

// Hash 使用 HMAC-SHA256 生成明文的哈希值。
// 警告：此 hash 依赖 encryption key。切换加密模式（如从 noop 切换到 aes 或更换 key）
// 会导致所有现有 key_hash 失效。如需切换加密模式，必须手动重新计算所有 key 的 hash。
func (s *aesService) Hash(plaintext string) string {
	if plaintext == "" {
		return ""
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(plaintext))
	return hex.EncodeToString(mac.Sum(nil))
}

// noopService 禁用加密的空实现
type noopService struct{}

func (s *noopService) Encrypt(plaintext string) (string, error) {
	return plaintext, nil
}

func (s *noopService) Decrypt(ciphertext string) (string, error) {
	return ciphertext, nil
}

// BatchDecrypt 空服务的批量解密 - 返回输入的恒等映射
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

// Hash 使用 SHA256 生成明文的哈希值（无需密钥）
func (s *noopService) Hash(plaintext string) string {
	if plaintext == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(hash[:])
}
