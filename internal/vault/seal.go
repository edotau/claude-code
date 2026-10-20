package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

type Key struct {
	Kid  string
	aead cipher.AEAD
	raw  string // the base64 value this key was parsed from; rotation retains it as-is (R4)
}

func parseKey(value string) (*Key, error) {
	trimmed := strings.TrimSpace(value)
	raw, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("vault: master key must be base64-encoded 32 bytes")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &Key{Kid: hex.EncodeToString(sum[:])[:16], aead: aead, raw: trimmed}, nil
}

func (k *Key) Seal(name, value string) []byte {
	return k.aead.Seal(nil, nil, []byte(value), []byte("claude-code/vault/v1/secrets/"+name))
}

func (k *Key) Open(name string, r Record) (string, error) {
	if r.Kid != k.Kid {
		return "", ErrWrongKey
	}
	v, err := k.aead.Open(nil, nil, r.Sealed, []byte("claude-code/vault/v1/secrets/"+name))
	return string(v), err
}
