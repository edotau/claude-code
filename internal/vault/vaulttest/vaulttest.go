package vaulttest

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
)

func Use(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	return dir
}

func Put(t testing.TB, bucket, key, val string) {
	t.Helper()
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(bucket, key, val) }); err != nil {
		t.Fatal(err)
	}
}

func Secret(t testing.TB, name, value string) {
	t.Helper()
	k, err := vault.EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	r := vault.Record{Sealed: k.Seal(name, value), Kid: k.Kid, Len: len(value), SetAt: time.Now()}
	if err = vault.Update(func(tx *vault.Tx) error { return tx.PutRecord(name, r) }); err != nil {
		t.Fatal(err)
	}
}
