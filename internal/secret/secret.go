// Package secret encrypts sensitive values (API tokens, tunnel tokens) at rest.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Box struct{ aead cipher.AEAD }

// Load builds a Box from an explicit passphrase (CTM_SECRET_KEY) or, when it is
// empty, from a random key persisted in dataDir/secret.key (created on first run).
func Load(passphrase, dataDir string) (*Box, error) {
	var key []byte
	if passphrase != "" {
		sum := sha256.Sum256([]byte(passphrase))
		key = sum[:]
	} else {
		path := filepath.Join(dataDir, "secret.key")
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			key, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
			if err != nil || len(key) != 32 {
				return nil, fmt.Errorf("%s is corrupt", path)
			}
		case errors.Is(err, os.ErrNotExist):
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(plain string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (b *Box) Open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", errors.New("invalid ciphertext")
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("cannot decrypt: wrong secret key?")
	}
	return string(plain), nil
}
