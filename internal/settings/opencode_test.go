package settings

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/edotau/claude-code/internal/providers"
	"os"
	"testing"
)

// fakeRouter is a VendorRouter whose only live method is the overlay models.
type fakeRouter struct {
	mainModel, smallModel string
	ok                    bool
}

func (fakeRouter) ResolveTarget(*providers.Registry, string, providers.Selection, bool) (providers.Target, string, error) {
	return providers.Target{}, "", errors.New("fake")
}

func (fakeRouter) Configure(context.Context, string, providers.Target, string) ([]string, []string, error) {
	return nil, nil, errors.New("fake")
}

func (f fakeRouter) OpencodeOverlayModels() (string, string, bool) {
	return f.mainModel, f.smallModel, f.ok
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cfg, tui, err := WriteOpencodeOverlay(fakeRouter{"fix/sonnet", "fix/haiku", true})
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
	if got := applyOpencodeTableModels(tmpl, fakeRouter{"x/a", "x/b", false}); string(got) != string(tmpl) {
		t.Errorf("not ok: applyOpencodeTableModels = %s, want the template unchanged", got)
	}
	if _, _, err := WriteOpencodeOverlay(nil); !errors.Is(err, errNoRouter) {
		t.Errorf("nil router: err = %v, want errNoRouter", err)
	}
}
