// Package seal encrypts secret material for storage with AES-256-GCM,
// keyed by SHA-256 of the server secret.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// AES implements store-style encryption of API keys at rest.
type AES struct {
	aead cipher.AEAD
}

// New derives the key from secret and returns an encryptor.
func New(secret string) (*AES, error) {
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &AES{aead: aead}, nil
}

func (a *AES) Seal(plaintext string) (string, error) {
	nonce := make([]byte, a.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := a.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

func (a *AES) Open(ciphertext string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	if len(raw) < a.aead.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, body := raw[:a.aead.NonceSize()], raw[a.aead.NonceSize():]
	plaintext, err := a.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}
