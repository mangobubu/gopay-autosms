package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
)

// Box provides authenticated encryption for persisted API/session secrets.
// The container persists the source secret alongside the database credentials.
type Box struct {
	aead cipher.AEAD
}

func New(secret string) (*Box, error) {
	if secret == "" {
		return nil, fmt.Errorf("secure: secret key is empty")
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("secure: create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secure: create GCM: %w", err)
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secure: generate nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plain, nil), nil
}

func (b *Box) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, nil
	}
	nonceSize := b.aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("secure: ciphertext is truncated")
	}
	plain, err := b.aead.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], nil)
	if err != nil {
		return nil, fmt.Errorf("secure: decrypt: %w", err)
	}
	return plain, nil
}
