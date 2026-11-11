package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

const encPrefix = "enc:"

// encryptField encrypts plaintext with AES-256-GCM using key.
// Returns the plaintext unchanged if key is nil or plaintext is empty.
// Output format: "enc:<base64(nonce+ciphertext)>"
func encryptField(key []byte, plaintext string) (string, error) {
	if len(key) == 0 || plaintext == "" {
		return plaintext, nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// decryptField decrypts a value produced by encryptField.
// If the value does not start with "enc:", it is returned as-is (plaintext pass-through
// allows reading values written before encryption was enabled).
func decryptField(key []byte, value string) (string, error) {
	if len(key) == 0 || !strings.HasPrefix(value, encPrefix) {
		return value, nil
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, encPrefix))
	if err != nil {
		return value, nil // malformed — treat as plaintext
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("gcm: %w", err)
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, data[:ns], data[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}
