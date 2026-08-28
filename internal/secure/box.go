package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// Box provides authenticated encryption for persisted API/session secrets.
// The container persists the source secret alongside the database credentials.
type Box struct {
	aead          cipher.AEAD
	blindIndexKey [sha256.Size]byte
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
	indexKey := hmac.New(sha256.New, key[:])
	_, _ = indexKey.Write([]byte("autosms/postgres/blind-index-key/v1"))
	box := &Box{aead: aead}
	copy(box.blindIndexKey[:], indexKey.Sum(nil))
	return box, nil
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

// BlindIndex returns a deterministic, purpose-separated lookup token without
// exposing low-entropy plaintext to offline dictionary checks. It is separate
// from Seal because database indexes must remain stable across process starts.
func (b *Box) BlindIndex(purpose string, value []byte) string {
	mac := hmac.New(sha256.New, b.blindIndexKey[:])
	_, _ = mac.Write([]byte(purpose))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(value)
	return hex.EncodeToString(mac.Sum(nil))
}
