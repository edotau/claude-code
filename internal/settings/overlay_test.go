package settings

import (
	"encoding/json"
	"reflect"
	"testing"
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
	if string(obj["tui"]) != `"default"` {
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
