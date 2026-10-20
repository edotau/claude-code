package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// secretsInfo is the HKDF info label for the secrets-sealing subkey; a future encrypted bucket
// gets its own label, deriving a distinct subkey from the same root.
const secretsInfo = "claude-code/vault/v1/secrets"

type Key struct {
	Kid  string
	aead cipher.AEAD
	raw  string // the base64 value this key was parsed from; rotation retains it as-is (R4)
}

// parseKey turns a base64 master-key value into a Key scoped to vaultID. Kid identifies the root
// itself (stable across vaults so retired-key accounts and matchKey stay vault-independent); the
// AEAD seals under K = HKDF(sha256, root, salt=vaultID, info=secretsInfo, 32) so a root reused
// across two vaults still yields distinct ciphertext keys, without changing who can decrypt.
func parseKey(value, vaultID string) (*Key, error) {
	trimmed := strings.TrimSpace(value)
	root, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil || len(root) != 32 {
		return nil, fmt.Errorf("vault: master key must be base64-encoded 32 bytes")
	}
	sub, err := hkdf.Key(sha256.New, root, []byte(vaultID), secretsInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(sub)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(root)
	return &Key{Kid: hex.EncodeToString(sum[:])[:16], aead: aead, raw: trimmed}, nil
}

func (k *Key) Seal(name, value string) []byte {
	return k.aead.Seal(nil, nil, []byte(value), []byte(secretsInfo+"/"+name))
}

func (k *Key) Open(name string, r Record) (string, error) {
	if r.Kid != k.Kid {
		return "", ErrWrongKey
	}
	v, err := k.aead.Open(nil, nil, r.Sealed, []byte(secretsInfo+"/"+name))
	return string(v), err
}
