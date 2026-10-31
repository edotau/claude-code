package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/defaults"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestApplyOverlaySwapsKnownKey: applyOverlay must replace a rendered value with the overlay's value for a
// key the catalog recognizes, leaving every other key untouched.
func TestApplyOverlaySwapsKnownKey(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	ov := map[string]json.RawMessage{"theme": json.RawMessage(`"dark"`)}
	out, err := applyOverlay(rendered, ov)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if string(obj["theme"]) != `"dark"` {
		t.Errorf("theme = %s, want \"dark\"", obj["theme"])
	}
	if string(obj["tui"]) != `"fullscreen"` {
		t.Errorf("unrelated key tui changed: %s", obj["tui"])
	}
}

// TestApplyOverlayCarriesAutoModeObject: autoMode is overlay-only (Section==""); applyOverlay must still
// place it into the rendered output with the object value intact, even when a string inside it contains
// a path-like token such as "<repo>".
func TestApplyOverlayCarriesAutoModeObject(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	want := json.RawMessage(`{"soft_deny":["$defaults"],"environment":"/Users/eric/<repo>/worktrees"}`)
	ov := map[string]json.RawMessage{"autoMode": want}
	out, err := applyOverlay(rendered, ov)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	var gotVal, wantVal any
	if err := json.Unmarshal(obj["autoMode"], &gotVal); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantVal); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Errorf("autoMode = %s, want %s", obj["autoMode"], want)
	}
}

// TestApplyOverlayRejectsUnknownKey: an overlay entry for a key the catalog does not recognize must error —
// a silently-accepted typo would never reach settings.json and the user would never know why.
func TestApplyOverlayRejectsUnknownKey(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	ov := map[string]json.RawMessage{"notARealSetting": json.RawMessage(`"x"`)}
	if _, err := applyOverlay(rendered, ov); err == nil {
		t.Error("applyOverlay with an unknown key = nil error, want rejection")
	}
}

// TestOverlayRoundTrip: SaveOverlay then LoadOverlay, in an isolated CLAUDE_CONFIG_DIR, must return the same
// entries, and RenderLive must apply them on top of the template render.
func TestOverlayRoundTrip(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	empty, err := LoadOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("LoadOverlay on a fresh config dir = %v, want empty", empty)
	}

	ov := map[string]json.RawMessage{"theme": json.RawMessage(`"light"`)}
	if err := SaveOverlay(ov); err != nil {
		t.Fatal(err)
	}

	got, err := LoadOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if string(got["theme"]) != `"light"` {
		t.Fatalf("LoadOverlay after SaveOverlay: theme = %s, want \"light\"", got["theme"])
	}

	live, err := RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(live, &obj); err != nil {
		t.Fatal(err)
	}
	if string(obj["theme"]) != `"light"` {
		t.Errorf("RenderLive theme = %s, want \"light\" from the overlay", obj["theme"])
	}
}

// TestOverlayRejectsInvalidValue: SaveOverlay must reject a value that fails the catalog entry's Validate —
// an overlay is only useful if it can never render settings.json into an invalid state.
func TestOverlayRejectsInvalidValue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ov := map[string]json.RawMessage{"cleanupPeriodDays": json.RawMessage(`0`)}
	if err := SaveOverlay(ov); err == nil {
		t.Error("SaveOverlay with cleanupPeriodDays=0 = nil error, want rejection")
	}
}

// TestDroppedRoundTripSortedDeduped: UpdateDropped stores sorted+deduped keys; an empty list deletes the entry.
func TestDroppedRoundTripSortedDeduped(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if got, err := LoadDropped(); err != nil || got != nil {
		t.Fatalf("fresh LoadDropped = %v, %v; want nil, nil", got, err)
	}
	if err := UpdateDropped(func([]string) []string { return []string{"zeta", "alpha", "zeta"} }); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDropped()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "zeta"}; !slices.Equal(got, want) {
		t.Fatalf("LoadDropped = %v, want %v", got, want)
	}
	if err := UpdateDropped(func([]string) []string { return nil }); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadDropped(); got != nil {
		t.Fatalf("after UpdateDropped(func([]string) []string { return nil }) LoadDropped = %v, want nil", got)
	}
}

// TestUpdateDroppedRefusesTemplateKeys: catalog keys and rendered top-level keys are never droppable.
func TestUpdateDroppedRefusesTemplateKeys(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rendered, &top); err != nil {
		t.Fatal(err)
	}
	keys := []string{"theme"}
	for k := range top {
		keys = append(keys, k)
	}
	for _, k := range keys {
		err := UpdateDropped(func([]string) []string { return []string{"foreignKey", k} })
		if err == nil || !strings.Contains(err.Error(), k+" is owned by the template") {
			t.Errorf("UpdateDropped(%q) err = %v, want the owned-by-template refusal", k, err)
		}
	}
	if got, _ := LoadDropped(); got != nil {
		t.Errorf("a refused save must not write; LoadDropped = %v", got)
	}
}

// TestOverlayAcceptsAnySchemaKey: a schema key outside the Catalog applies at top level, a schema env variable
// lands inside env, and a key the schema lacks is still refused.
func TestOverlayAcceptsAnySchemaKey(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	out, err := applyOverlay(rendered, map[string]json.RawMessage{
		"wheelScrollAccelerationEnabled": json.RawMessage("false"),
		"DISABLE_TELEMETRY":              json.RawMessage(`"1"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	o, _ := parseObject(out)
	env, _ := parseObject(o.vals["env"])
	if string(o.vals["wheelScrollAccelerationEnabled"]) != "false" || string(env.vals["DISABLE_TELEMETRY"]) != `"1"` {
		t.Errorf("overlay keys not applied:\n%s", out)
	}
	if _, ok := o.vals["DISABLE_TELEMETRY"]; ok {
		t.Error("env variable leaked to the top level")
	}
	if _, err := applyOverlay(rendered, map[string]json.RawMessage{"noSuchKey": json.RawMessage("1")}); err == nil {
		t.Error("unknown key accepted")
	}
}

// TestWriteRepoDefaultsKeepsSchemaFirst: staged keys land in the file (env variables under env), $schema stays
// first even from a file that had it misplaced, and a staged key outside the schema is refused.
func TestWriteRepoDefaultsKeepsSchemaFirst(t *testing.T) {
	dir := vaulttest.Use(t)
	file := filepath.Join(dir, filepath.FromSlash(RepoDefaultsRel))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	o, err := parseObject(defaults.Claude)
	if err != nil {
		t.Fatal(err)
	}
	o.keys = append(o.keys[1:], o.keys[0]) // $schema last
	seed, _ := o.marshal()
	if err := os.WriteFile(file, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	staged := map[string]json.RawMessage{
		"wheelScrollAccelerationEnabled": json.RawMessage("false"),
		"DISABLE_TELEMETRY":              json.RawMessage(`"1"`),
	}
	if _, err := WriteRepoDefaults(map[string]json.RawMessage{"noSuchKey": json.RawMessage("1")}); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := WriteRepoDefaults(staged); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file)
	back, err := parseObject(got)
	if err != nil {
		t.Fatal(err)
	}
	if back.keys[0] != "$schema" {
		t.Errorf("first key = %q, want $schema", back.keys[0])
	}
	if string(back.vals["wheelScrollAccelerationEnabled"]) != "false" || !strings.Contains(string(back.vals["env"]), `"DISABLE_TELEMETRY": "1"`) {
		t.Errorf("staged keys missing:\n%s", got)
	}
}
