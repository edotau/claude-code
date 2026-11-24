package agentconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestOpencodeOverlayTemplatesParse pins the embedded templates as valid JSON.
func TestOpencodeOverlayTemplatesParse(t *testing.T) {
	for name, body := range map[string][]byte{
		"opencode.json":     assets.OpencodeConfigTemplate(),
		"opencode-tui.json": assets.OpencodeTUITemplate(),
	} {
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("%s: not valid JSON: %v", name, err)
		}
	}
}

// The cfg file's model/small_model come from the routing table; every other field matches the embedded template.
func TestWriteOpencodeOverlayWritesBothFiles(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"routing":{"cells":{"opencode":{"sonnet":{"model":"github-copilot/fix-sonnet"},"haiku":{"model":"github-copilot/fix-haiku"}}}}}`)
	cfg, tui, err := WriteOpencodeOverlay(router.OpencodeTableModels)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cfg, tui} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", p, fi.Mode().Perm())
		}
	}
	tmpl := assets.OpencodeConfigTemplate()
	var want map[string]any
	if err := json.Unmarshal(tmpl, &want); err != nil {
		t.Fatal(err)
	}
	want["model"], want["small_model"] = "github-copilot/fix-sonnet", "github-copilot/fix-haiku"
	want["skills"] = map[string]any{"paths": []any{filepath.Join(paths.ConfigDir(), "skills")}}

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

// TestApplyOpencodeTableModelsKeepsUnparsableTemplate checks a body that is not a JSON object comes back unchanged.
func TestApplyOpencodeTableModelsKeepsUnparsableTemplate(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Use(t)
	bad := []byte("not json")
	if got := applyOpencodeTableModels(bad, router.OpencodeTableModels); string(got) != string(bad) {
		t.Errorf("applyOpencodeTableModels = %s, want the input unchanged", got)
	}
}

// Each skill is reachable by one root: the config dir's own, then only the ~/.agents/skills entries it lacks.
func TestOpencodeSkillRootsListEachSkillOnce(t *testing.T) {
	cfgDir, home := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	own, shared := filepath.Join(cfgDir, "skills"), filepath.Join(home, ".agents", "skills")
	for _, d := range []string{filepath.Join(own, "dup"), shared, filepath.Join(shared, "only"), filepath.Join(shared, "b-only")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shared, "file.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(own, "dup"), filepath.Join(shared, "dup")); err != nil {
		t.Log("symlinks unavailable, using a copy:", err)
		if err := os.Mkdir(filepath.Join(shared, "dup"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{own, filepath.Join(shared, "b-only"), filepath.Join(shared, "only")}
	got := opencodeSkillRoots()
	if !slices.Equal(got, want) {
		t.Errorf("roots = %v, want %v", got, want)
	}
}
