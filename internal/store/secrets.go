package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// The master key file holds a random AES-256 key, base64-encoded (standard
// alphabet, padded) and followed by a newline. Losing it makes every secret
// setting unreadable, so its format must never change.
const masterKeySize = 32

// secretPrefix marks an encrypted setting. The stored value is
// secretPrefix + base64(nonce || AES-GCM ciphertext), sealed with the
// setting's key as additional data so a value cannot be moved to another
// setting.
const secretPrefix = "enc:"

// newAEAD returns the AES-GCM cipher of the master key at keyPath, creating
// the key when missing.
func newAEAD(keyPath string) (cipher.AEAD, error) {
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// loadOrCreateKey reads the master key at path, or writes a new one (0600)
// when there is none.
func loadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return createKey(path)
	}
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != masterKeySize {
		return nil, errors.New("invalid master key file " + path)
	}
	return key, nil
}

// createKey writes a new random master key at path.
func createKey(path string) ([]byte, error) {
	key := make([]byte, masterKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), privateFileMode); err != nil {
		return nil, err
	}
	return key, nil
}

// SetSecret encrypts and stores a setting.
func (s *Store) SetSecret(key, value string) error {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(value), []byte(key))
	return s.Set(key, secretPrefix+base64.StdEncoding.EncodeToString(sealed))
}

// GetSecret decrypts a setting stored with SetSecret ("" when unset).
func (s *Store) GetSecret(key string) (string, error) {
	raw := s.Get(key, "")
	if raw == "" {
		return "", nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, secretPrefix))
	if err != nil {
		return "", err
	}
	ns := s.aead.NonceSize()
	if len(b) < ns {
		return "", errors.New("ciphertext too short")
	}
	plain, err := s.aead.Open(nil, b[:ns], b[ns:], []byte(key))
	if err != nil {
		return "", fmt.Errorf("decrypt %s: %w", key, err)
	}
	return string(plain), nil
}
