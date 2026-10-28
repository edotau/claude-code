package providers

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestRenderPinsSorted(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Pins, "B", "2")
	vaulttest.Put(t, vault.Pins, "A", "1")
	got, err := RenderPins()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("# Managed by `claude-code switch` / `claude-code models --pin`.\nA=1\nB=2\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("RenderPins() = %q, want %q", got, want)
	}
}

func TestRenderPinsAbsent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, existing := range []bool{false, true} {
		if existing {
			vaulttest.Put(t, vault.Env, "PLAIN", "value")
		}
		if _, err := RenderPins(); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("RenderPins (existing vault %v) error = %v, want fs.ErrNotExist", existing, err)
		}
	}
}

func TestSelectSurfacesUnreadableVault(t *testing.T) {
	reg := selectTestRegistry(t)
	vaulttest.Put(t, vault.Meta, "schema", "999")
	for _, env := range []string{"", "openai"} {
		t.Setenv(PinProfile, env)
		if _, err := Select(reg, ""); err == nil || !strings.Contains(err.Error(), "vault: pins unreadable") {
			t.Errorf("Select (env %q) error = %v, want vault: pins unreadable", env, err)
		}
	}
	if got := SessionProfile(); got != "openai" {
		t.Errorf("SessionProfile() = %q, want env value openai despite unreadable vault", got)
	}
	if _, err := PinDiff(map[string]string{PinProfile: "gemini"}); err == nil {
		t.Error("PinDiff must return the vault read error")
	}
}

func TestSetProviderModelConcurrentSlots(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	start := make(chan struct{})
	results := make(chan error, 2)
	for slot, model := range map[string]string{"opus": "a", "haiku": "b"} {
		go func() {
			<-start
			results <- SetProviderModel("openai", slot, model)
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Errorf("SetProviderModel: %v", err)
		}
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Providers["openai"]
	if p.Models["opus"] != "a" || p.Models["haiku"] != "b" {
		t.Fatalf("concurrent model edits lost a slot: %v, want opus=a and haiku=b", p.Models)
	}
}

func TestSetProviderFieldsPreservesPreviousDoc(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := SetProviderFields("openai", map[string]any{"base_url": "https://first.example/v1"}); err != nil {
		t.Fatal(err)
	}
	first, err := OverlayDoc()
	if err != nil {
		t.Fatal(err)
	}
	if err := SetProviderFields("openai", map[string]any{"base_url": "https://second.example/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		prev, ok := tx.Get(vault.Overlay, "providers.prev")
		if !ok || !bytes.Equal([]byte(prev), first) {
			t.Errorf("providers.prev = %q, present %v; want %q", prev, ok, first)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, err := OverlayDoc()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(current, first) {
		t.Error("second write did not update overlay/providers")
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Providers["openai"].BaseURL; got != "https://second.example/v1" {
		t.Errorf("second base_url = %q", got)
	}
}

func TestSetProviderFieldsRefusesInvalidOverlay(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const prior = `{"providers":{"openai":{"openai":{"kind":"openai","base_url":"https://example.com/v1","auth":{"type":"none"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", prior)
	vaulttest.Put(t, vault.Overlay, "providers.prev", "{}")
	if err := Validate([]byte(prior)); err != nil {
		t.Fatalf("valid initial overlay: %v", err)
	}
	if err := SetProviderFields("openai", map[string]any{"base_url": ""}); err == nil {
		t.Fatal("empty base_url with no routes must fail validation")
	}
	if err := vault.View(func(tx *vault.Tx) error {
		for key, want := range map[string]string{"providers": prior, "providers.prev": "{}"} {
			if got, ok := tx.Get(vault.Overlay, key); !ok || got != want {
				t.Errorf("refused edit changed %s: %q, present %v; want %q", key, got, ok, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSetPinsEmptyDeletesKey(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(PinProfile, "")
	if err := SetPins(map[string]string{PinProfile: "openai", PinModel: "keep"}); err != nil {
		t.Fatal(err)
	}
	diff, err := PinDiff(map[string]string{PinProfile: ""})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{PinProfile + ": openai \u2192 (unset)"}; !reflect.DeepEqual(diff, want) {
		t.Fatalf("deletion diff = %q, want %q", diff, want)
	}
	if err := SetPins(map[string]string{PinProfile: ""}); err != nil {
		t.Fatal(err)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if got, ok := tx.Get(vault.Pins, PinProfile); ok {
			t.Errorf("deleted pin still present: %q", got)
		}
		if got, ok := tx.Get(vault.Pins, PinModel); !ok || got != "keep" {
			t.Errorf("unrelated pin = %q, present %v; want keep", got, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := Pin(PinProfile); got != "" {
		t.Errorf("deleted Pin() = %q, want empty", got)
	}
	diff, err = PinDiff(map[string]string{PinProfile: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{PinProfile + ": (unset) \u2192 openai"}; !reflect.DeepEqual(diff, want) {
		t.Fatalf("diff after deletion = %q, want %q", diff, want)
	}
}

func TestPinDiffNeverWritesVault(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[existing], func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			var before fs.FileInfo
			if existing {
				vaulttest.Put(t, vault.Pins, "A", "1")
				past := time.Unix(1, 0)
				if err := os.Chtimes(vault.Path(), past, past); err != nil {
					t.Fatal(err)
				}
				var err error
				before, err = os.Stat(vault.Path())
				if err != nil {
					t.Fatal(err)
				}
			}
			diff, err := PinDiff(map[string]string{"B": "2"})
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"B: (unset) \u2192 2"}; !reflect.DeepEqual(diff, want) {
				t.Fatalf("PinDiff = %q, want %q", diff, want)
			}
			after, err := os.Stat(vault.Path())
			if !existing {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("PinDiff created a vault: stat error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
				t.Fatalf("PinDiff changed vault mtime/size: %v/%d -> %v/%d",
					before.ModTime(), before.Size(), after.ModTime(), after.Size())
			}
		})
	}
}

func TestLoadMalformedVaultOverlay(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "providers", "{not json")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "vault overlay/providers") {
		t.Fatalf("Load() error = %v, want vault overlay/providers", err)
	}
}
