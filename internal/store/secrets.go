package store

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(key) != 32 {
			return nil, errors.New("invalid master key file " + path)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
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
	return s.Set(key, "enc:"+base64.StdEncoding.EncodeToString(sealed))
}

// GetSecret decrypts a setting stored with SetSecret.
func (s *Store) GetSecret(key string) (string, error) {
	raw := s.Get(key, "")
	if raw == "" {
		return "", nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "enc:"))
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
