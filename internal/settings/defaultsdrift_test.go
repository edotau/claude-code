package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestDefaultsDrift(t *testing.T) {
	vaulttest.Use(t)
	file := filepath.Join(paths.ConfigDir(), "settings.json")
	if keys, err := DefaultsDrift(); err != nil || keys != nil {
		t.Fatalf("no file: %v, %v; want none", keys, err)
	}
	rendered, err := RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, rendered, 0o600); err != nil {
		t.Fatal(err)
	}
	if keys, err := DefaultsDrift(); err != nil || len(keys) != 0 {
		t.Fatalf("clean file: %v, %v; want none", keys, err)
	}
	o, _ := parseObject(rendered)
	o.set("model", json.RawMessage(`"drifted"`))
	o.set("zzExtra", json.RawMessage(`1`))
	o.set("modelSettings", json.RawMessage(`{"a":1}`)) // runtime key: ignored
	env, _ := parseObject(orEmpty(o.vals["env"]))
	env.set("ZZ_LIVE_ONLY", json.RawMessage(`"1"`))
	raw, _ := env.marshal()
	o.set("env", raw)
	edited, _ := o.marshal()
	if err := os.WriteFile(file, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{"env.ZZ_LIVE_ONLY", "model", "zzExtra"}
	if keys, err := DefaultsDrift(); err != nil || !slices.Equal(keys, want) {
		t.Fatalf("drifted: %v, %v; want %v", keys, err, want)
	}
	if err := os.WriteFile(file, []byte(`{nope`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DefaultsDrift(); err == nil {
		t.Fatal("invalid JSON must be an error")
	}
}
