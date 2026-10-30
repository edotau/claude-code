package settings

import (
	"encoding/json"
	"os"
	"testing"
)

// fixtureOverlayModels swaps the OpencodeOverlayModels seam (vendors sets it in production) for fixed models.
func fixtureOverlayModels(t *testing.T, mainModel, smallModel string, ok bool) {
	t.Helper()
	orig := OpencodeOverlayModels
	OpencodeOverlayModels = func() (string, string, bool) { return mainModel, smallModel, ok }
	t.Cleanup(func() { OpencodeOverlayModels = orig })
}

// TestOpencodeOverlayTemplatesParse pins the embedded templates as valid JSON.
func TestOpencodeOverlayTemplatesParse(t *testing.T) {
	for _, name := range []string{"template/opencode.json", "template/opencode-tui.json"} {
		body, err := opencodeOverlayFS.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("%s: not valid JSON: %v", name, err)
		}
	}
}

// TestWriteOpencodeOverlayWritesBothFiles checks the two overlay files land at state/harness/opencode
// with mode 0600, and that the cfg file's model/small_model come from the routing table, every other
// field unchanged from the embedded template (the table-driven contract applyOpencodeTableModels keeps).
func TestWriteOpencodeOverlayWritesBothFiles(t *testing.T) {
	fixtureOverlayModels(t, "fix/sonnet", "fix/haiku", true)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cfg, tui, err := WriteOpencodeOverlay()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cfg, tui} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", p, fi.Mode().Perm())
		}
	}
	tmpl, err := opencodeOverlayFS.ReadFile("template/opencode.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(tmpl, &want); err != nil {
		t.Fatal(err)
	}
	want["model"], want["small_model"] = "fix/sonnet", "fix/haiku"

	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("opencode.json: not valid JSON: %v", err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("opencode.json = %s, want %s (model/small_model from the table, rest from the template)", gotJSON, wantJSON)
	}
}

// TestApplyOpencodeTableModelsFallsBackOnLoadError checks a failed table read (seam not ok, or unset) leaves the
// template's own hard-coded model/small_model untouched, the one fallback use.
func TestApplyOpencodeTableModelsFallsBackOnLoadError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tmpl, err := opencodeOverlayFS.ReadFile("template/opencode.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, set := range map[string]func(){
		"not ok": func() { fixtureOverlayModels(t, "x/a", "x/b", false) },
		"unset": func() {
			orig := OpencodeOverlayModels
			OpencodeOverlayModels = nil
			t.Cleanup(func() { OpencodeOverlayModels = orig })
		},
	} {
		set()
		if got := applyOpencodeTableModels(tmpl); string(got) != string(tmpl) {
			t.Errorf("%s: applyOpencodeTableModels = %s, want the template unchanged", name, got)
		}
	}
}
