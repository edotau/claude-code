package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestDiffInvalidJSON: malformed existing JSON (a trailing comma) must surface as an error wrapping
// ErrInvalidJSON, not a silent "no drift" or a generic error the caller can't branch on.
func TestDiffInvalidJSON(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Diff([]byte(`{"a":1,}`), rendered)
	if err == nil {
		t.Fatal("Diff on trailing-comma JSON = nil error, want ErrInvalidJSON")
	}
	if !errors.Is(err, ErrInvalidJSON) {
		t.Errorf("Diff error = %v, want errors.Is(ErrInvalidJSON)", err)
	}
}

// TestDiffDetectsChangedValue: a hand-edited value that differs from the rendered template must be reported
// in differs, by key.
func TestDiffDetectsChangedValue(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rendered, &obj); err != nil {
		t.Fatal(err)
	}
	obj["hooks"] = json.RawMessage(`{}`)
	obj["theme"] = json.RawMessage(`"light"`)
	existing, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}

	differs, foreign, err := Diff(existing, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(differs, []string{"hooks"}) { // only hooks are refreshed; theme is the user's
		t.Errorf("differs = %v, want [hooks]", differs)
	}
	if len(foreign) != 0 {
		t.Errorf("foreign = %v, want none", foreign)
	}
}

// TestDiffDetectsForeignKey: a key present in the existing file but absent from the rendered template is
// foreign — e.g. something /config wrote that the harness does not own — not drift.
func TestDiffDetectsForeignKey(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rendered, &obj); err != nil {
		t.Fatal(err)
	}
	obj["verbose"] = json.RawMessage(`true`)
	existing, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}

	differs, foreign, err := Diff(existing, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(foreign, "verbose") {
		t.Errorf("foreign = %v, want it to contain %q", foreign, "verbose")
	}
	if len(differs) != 0 {
		t.Errorf("differs = %v, want none", differs)
	}
}

// TestDiffCleanAfterMerge: a file produced by Merge(existing, rendered) must diff clean against that same
// rendered template — formatting differences (key order, whitespace) must not register as drift.
func TestDiffCleanAfterMerge(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	merged, _, err := Merge([]byte(`{"verbose":true}`), rendered)
	if err != nil {
		t.Fatal(err)
	}

	differs, foreign, err := Diff(merged, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(differs) != 0 {
		t.Errorf("differs = %v, want none (Merge output against its own rendered template)", differs)
	}
	if !slices.Contains(foreign, "verbose") {
		t.Errorf("foreign = %v, want it to still contain the foreign key Merge preserved", foreign)
	}
}

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
