package agentconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/router"
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
	mkSkillDir(t, filepath.Join(paths.ConfigDir(), "skills"), false)
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
	cfgDir, home, work := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	own, shared, proj := filepath.Join(cfgDir, "skills"), filepath.Join(home, ".agents", "skills"), filepath.Join(work, ".agents", "skills")
	mkSkillDir(t, filepath.Join(own, "dup"), true)
	mkSkillDir(t, filepath.Join(own, ".hid"), true)
	mkSkillDir(t, filepath.Join(own, "empty"), false)
	mkSkillDir(t, filepath.Join(shared, "empty"), true)
	mkSkillDir(t, filepath.Join(shared, "only"), true)
	mkSkillDir(t, filepath.Join(shared, "none"), false)
	mkSkillDir(t, filepath.Join(proj, "p"), true)
	want := []string{own, filepath.Join(own, ".hid"), filepath.Join(shared, "empty"), filepath.Join(shared, "only"), filepath.Join(proj, "p")}
	if err := os.Symlink(filepath.Join(own, "dup"), filepath.Join(shared, "dup")); err != nil {
		t.Log("symlinks unavailable, copy used:", err)
		mkSkillDir(t, filepath.Join(shared, "dup"), true)
		want = []string{own, filepath.Join(own, ".hid"), filepath.Join(shared, "empty"), filepath.Join(shared, "only"), filepath.Join(proj, "p")}
	}
	if got := OpencodeSkillRoots(work); !slices.Equal(got, want) {
		t.Errorf("roots = %v, want %v", got, want)
	}
}

func mkSkillDir(t *testing.T, dir string, withSkill bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if withSkill {
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
