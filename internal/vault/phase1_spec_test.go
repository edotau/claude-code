package vault_test

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func specMasterKey(t *testing.T) *vault.Key {
	t.Helper()
	key, source, err := vault.MasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if key == nil || source != "env" {
		t.Fatalf("MasterKey = %v, %q; want non-nil env key", key, source)
	}
	return key
}

func TestSealOpenRoundTrip(t *testing.T) {
	vaulttest.Use(t)
	key := specMasterKey(t)
	for _, value := range []string{"", "representative-token", "line one\nline two\x00end"} {
		t.Run(fmt.Sprintf("bytes_%d", len(value)), func(t *testing.T) {
			sealed := key.Seal("TOKEN_A", value)
			if len(sealed) != len(value)+28 {
				t.Errorf("sealed length = %d, want %d (96-bit nonce + GCM tag)", len(sealed), len(value)+28)
			}
			got, err := key.Open("TOKEN_A", vault.Record{Sealed: sealed, Kid: key.Kid})
			if err != nil || got != value {
				t.Fatalf("Open round trip = %q, %v; want %q, nil", got, err, value)
			}
		})
	}
	vaulttest.Secret(t, "STORED_TOKEN", "stored-round-trip")
	if err := vault.View(func(tx *vault.Tx) error {
		record, ok := tx.Record("STORED_TOKEN")
		if !ok {
			t.Fatal("vaulttest.Secret did not persist the record")
		}
		got, err := key.Open("STORED_TOKEN", record)
		if err != nil || got != "stored-round-trip" {
			t.Errorf("stored round trip = %q, %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSealOpenRejectsAADSwap(t *testing.T) {
	vaulttest.Use(t)
	key := specMasterKey(t)
	record := vault.Record{Sealed: key.Seal("TOKEN_A", "bound-to-A"), Kid: key.Kid}
	if got, err := key.Open("TOKEN_A", record); err != nil || got != "bound-to-A" {
		t.Fatalf("same-name control failed: got=%q err=%v", got, err)
	}
	if got, err := key.Open("TOKEN_B", record); err == nil {
		t.Fatalf("AAD swap accepted record sealed for TOKEN_A as TOKEN_B: got=%q", got)
	}
	rawKey, err := base64.StdEncoding.DecodeString(os.Getenv("HARNESS_VAULT_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(rawKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		t.Fatal(err)
	}
	got, err := aead.Open(nil, nil, record.Sealed, []byte("claude-code/vault/v1/secrets/TOKEN_A"))
	if err != nil || string(got) != "bound-to-A" {
		t.Fatalf("ciphertext is not bound to the specified AAD prefix and name: got=%q err=%v", got, err)
	}
}

func TestSealOpenWrongKid(t *testing.T) {
	vaulttest.Use(t)
	key := specMasterKey(t)
	record := vault.Record{Sealed: key.Seal("TOKEN_A", "value"), Kid: "not-" + key.Kid}
	if _, err := key.Open("TOKEN_A", record); !errors.Is(err, vault.ErrWrongKey) {
		t.Fatalf("Open with wrong kid = %v, want ErrWrongKey", err)
	}
}

func TestViewMissingVaultIsEmptyWithoutCreatingFile(t *testing.T) {
	dir := vaulttest.Use(t)
	wantPath := filepath.Join(dir, "state", "vault.db")
	if got := vault.Path(); got != wantPath {
		t.Fatalf("Path = %q, want %q", got, wantPath)
	}
	called := false
	if err := vault.View(func(tx *vault.Tx) error {
		called = true
		for _, bucket := range []string{vault.Pins, vault.Env, vault.Overlay, vault.Secrets, vault.Meta} {
			if value, ok := tx.Get(bucket, "absent"); ok || value != "" {
				t.Errorf("missing %s Get = %q, %t", bucket, value, ok)
			}
			if len(tx.All(bucket)) != 0 || len(tx.Names(bucket)) != 0 {
				t.Errorf("missing %s is not empty", bucket)
			}
		}
		if record, ok := tx.Record("absent"); ok || !reflect.DeepEqual(record, vault.Record{}) {
			t.Errorf("missing Record = %+v, %t", record, ok)
		}
		if len(tx.Records()) != 0 {
			t.Error("missing vault has records")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("View did not call the callback on a missing vault")
	}
	if _, err := os.Stat(wantPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("View created a vault or stat failed unexpectedly: %v", err)
	}
}

func TestConcurrentUpdatePreservesEightKeys(t *testing.T) {
	vaulttest.Use(t)
	const writers = 8
	start := make(chan struct{})
	results := make(chan error, writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			<-start
			results <- vault.Update(func(tx *vault.Tx) error {
				return tx.Put(vault.Pins, fmt.Sprintf("key-%d", i), fmt.Sprintf("value-%d", i))
			})
		}(i)
	}
	close(start)
	for i := 0; i < writers; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("concurrent Update: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent Update did not finish within writer timeout")
		}
	}
	if err := vault.View(func(tx *vault.Tx) error {
		all := tx.All(vault.Pins)
		if len(all) != writers {
			t.Errorf("got %d keys, want %d: %v", len(all), writers, all)
		}
		for i := 0; i < writers; i++ {
			name, want := fmt.Sprintf("key-%d", i), fmt.Sprintf("value-%d", i)
			if got, ok := tx.Get(vault.Pins, name); !ok || got != want {
				t.Errorf("Get(%q) = %q, %t; want %q, true", name, got, ok, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestViewWaitsForHeldUpdate(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Pins, "held", "before")
	held, release := make(chan struct{}), make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- vault.Update(func(tx *vault.Tx) error {
			close(held)
			<-release
			return tx.Put(vault.Pins, "held", "after")
		})
	}()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	select {
	case <-held:
	case err := <-writeDone:
		t.Fatalf("Update ended before acquiring transaction: %v", err)
	case <-time.After(6 * time.Second):
		t.Fatal("Update never acquired the write transaction")
	}
	readStarted, readDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(readStarted)
		readDone <- vault.View(func(tx *vault.Tx) error {
			if got, ok := tx.Get(vault.Pins, "held"); !ok || got != "after" {
				return fmt.Errorf("reader saw %q, %t; want committed value after, true", got, ok)
			}
			return nil
		})
	}()
	<-readStarted
	select {
	case err := <-readDone:
		t.Fatalf("View returned while write transaction was held: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	released = true
	for name, done := range map[string]<-chan error{"writer": writeDone, "reader": readDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s failed after release: %v", name, err)
			}
		case <-time.After(6 * time.Second):
			t.Errorf("%s did not finish after release", name)
		}
	}
}

func TestViewRejectsNewerSchema(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Meta, "schema", "2")
	err := vault.View(func(tx *vault.Tx) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "newer claude-code") {
		t.Fatalf("View schema 2 = %v, want error containing newer claude-code", err)
	}
}
