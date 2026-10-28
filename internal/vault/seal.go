package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// Each encrypted collection derives a distinct subkey from the same vault-scoped root.
const secretsInfo = "claude-code/vault/v1/secrets"
const settingsBackupsInfo = "claude-code/vault/v1/settings-backups"

type Key struct {
	Kid         string
	aead        cipher.AEAD
	backupsAEAD cipher.AEAD
	raw         string // the base64 value this key was parsed from; rotation retains it as-is (R4)
}

// parseKey turns a base64 master-key value into a Key scoped to vaultID. Kid identifies the root
// itself (stable across vaults so retired-key accounts and matchKey stay vault-independent); the
// AEAD seals under K = HKDF(sha256, root, salt=vaultID, info=secretsInfo, 32) so a root reused
// across two vaults still yields distinct ciphertext keys, without changing who can decrypt.
func parseKey(value, vaultID string) (*Key, error) {
	trimmed := strings.TrimSpace(value)
	root, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil || len(root) != 32 {
		return nil, errors.New("vault: master key must be base64-encoded 32 bytes")
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
	backupSub, err := hkdf.Key(sha256.New, root, []byte(vaultID), settingsBackupsInfo, 32)
	if err != nil {
		return nil, err
	}
	backupBlock, err := aes.NewCipher(backupSub)
	if err != nil {
		return nil, err
	}
	backupAEAD, err := cipher.NewGCMWithRandomNonce(backupBlock)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(root)
	return &Key{Kid: hex.EncodeToString(sum[:])[:16], aead: aead, backupsAEAD: backupAEAD, raw: trimmed}, nil
}

// Raw returns the base64-encoded master key value k was parsed from — the same encoding
// HARNESS_VAULT_KEY accepts. Callers must only print it through an explicit, audited reveal path.
func (k *Key) Raw() string { return k.raw }

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

// SealSettingsBackup encrypts a snapshot without sharing the credential collection's key or AAD.
func (k *Key) SealSettingsBackup(name, value string) []byte {
	return k.backupsAEAD.Seal(nil, nil, []byte(value), []byte(settingsBackupsInfo+"/"+name))
}

func (k *Key) openIn(bucket, name string, r Record) (string, error) {
	if bucket == Secrets {
		return k.Open(name, r)
	}
	if r.Kid != k.Kid {
		return "", ErrWrongKey
	}
	v, err := k.backupsAEAD.Open(nil, nil, r.Sealed, []byte(settingsBackupsInfo+"/"+name))
	return string(v), err
}
