package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

const (
	KeySize   = 32
	NonceSize = 12
)

type Service struct {
	aead cipher.AEAD
}

func NewOptionalFromBase64(encoded string) (*Service, error) {
	if encoded == "" {
		return nil, nil
	}

	return NewFromBase64(encoded)
}

func NewFromBase64(encoded string) (*Service, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode base64 master key: %w", err)
	}

	if len(key) != KeySize {
		return nil, fmt.Errorf("master key must decode to %d bytes, got %d", KeySize, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create aes cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create aes-gcm cipher: %w", err)
	}

	return &Service{aead: aead}, nil
}

func (s *Service) EncryptString(plaintext string) (string, error) {
	if s == nil {
		return "", errors.New("secrets service is nil")
	}

	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := s.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (s *Service) DecryptString(encoded string) (string, error) {
	if s == nil {
		return "", errors.New("secrets service is nil")
	}

	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}

	if len(payload) < NonceSize {
		return "", fmt.Errorf("ciphertext too short: need at least %d bytes, got %d", NonceSize, len(payload))
	}

	nonce := payload[:NonceSize]
	ciphertext := payload[NonceSize:]

	plaintext, err := s.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt ciphertext: %w", err)
	}

	return string(plaintext), nil
}
