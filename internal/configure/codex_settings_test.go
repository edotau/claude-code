package configure

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/codexsettings"
	"github.com/edotau/claude-code/internal/generate"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func codexSettingsFixture(t *testing.T) (string, string) {
	t.Helper()
	vaulttest.Use(t)
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# native preferences\nmodel_verbosity = \"high\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return home, path
}

func codexSettingsDialog(home, input string) (*dialog, *bytes.Buffer) {
	in := strings.NewReader(input)
	out := &bytes.Buffer{}
	return &dialog{o: Options{Home: home, In: in}, br: bufio.NewReader(in), out: out}, out
}

func TestCodexSettingsMenuStagesUntilSave(t *testing.T) {
	for _, save := range []bool{false, true} {
		name, input := "cancel", "2\n1\n\n"
		if save {
			name, input = "save", "2\n1\n3\n"
		}
		t.Run(name, func(t *testing.T) {
			home, path := codexSettingsFixture(t)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			others := map[string]string{
				filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"): "{\"foreign\":true}\n",
				filepath.Join(home, ".copilot", "settings.json"):               "{\"theme\":\"dim\"}\n",
				filepath.Join(home, ".gemini", "settings.json"):                "{\"foreign\":true}\n",
			}
			for p, contents := range others {
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			d, out := codexSettingsDialog(home, input)
			if !runCodexSettingsMenu(d, map[string]string{}, map[string]string{"model_verbosity": "high"}) {
				t.Fatalf("menu failed: %s", out.String())
			}
			ov, err := codexsettings.LoadOverlay()
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if save {
				if ov["model_verbosity"] != "low" || !strings.Contains(string(after), `model_verbosity = "low"`) {
					t.Fatalf("saved preference missing: overlay=%v native=%s", ov, after)
				}
				backup, found, err := codexsettings.LatestBackup(path)
				if err != nil || !found || !bytes.Equal(backup.Data, before) {
					t.Fatalf("original native settings not backed up: found=%v err=%v", found, err)
				}
			} else if len(ov) != 0 || !bytes.Equal(before, after) {
				t.Fatalf("cancel persisted staged edit: overlay=%v native=%s", ov, after)
			}
			if d.changed {
				t.Error("Codex-only save must not request a global agent rerender")
			}
			for p, want := range others {
				got, err := os.ReadFile(p)
				if err != nil || string(got) != want {
					t.Errorf("other agent file changed: %s (err=%v)", p, err)
				}
			}
		})
	}
}

func TestCodexSettingsReportsNeverWrite(t *testing.T) {
	for _, mode := range []string{"report", "check", "dry-run", "piped"} {
		t.Run(mode, func(t *testing.T) {
			home, path := codexSettingsFixture(t)
			if err := codexsettings.SaveOverlay(map[string]string{"model_verbosity": "low"}); err != nil {
				t.Fatal(err)
			}
			nativeBefore, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			vaultBefore, err := os.ReadFile(vault.Path())
			if err != nil {
				t.Fatal(err)
			}
			input := strings.NewReader("2\n1\n3\n")
			var out bytes.Buffer
			o := Options{Home: home, In: input, Out: &out, Check: mode == "check", DryRun: mode == "dry-run", Interactive: mode == "piped"}
			rc := Run(o, "codex")
			want := 0
			if mode == "check" {
				want = 1
			}
			if rc != want || !strings.Contains(out.String(), "live differs") {
				t.Fatalf("report rc=%d want=%d output=%s", rc, want, out.String())
			}
			if input.Len() != len("2\n1\n3\n") {
				t.Error("read-only report consumed menu input")
			}
			nativeAfter, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			vaultAfter, err := os.ReadFile(vault.Path())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(nativeBefore, nativeAfter) || !bytes.Equal(vaultBefore, vaultAfter) {
				t.Error("read-only report changed native config or vault")
			}
		})
	}
}

func TestCodexSettingsApplyFailureIsReported(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".codex"), []byte("blocked native directory"), 0600); err != nil {
		t.Fatal(err)
	}
	d, out := codexSettingsDialog(home, "2\n1\n3\n")
	if runCodexSettingsMenu(d, map[string]string{}, map[string]string{}) {
		t.Fatal("menu reported success after native apply failed")
	}
	if !strings.Contains(out.String(), "preferences saved; native apply failed") {
		t.Fatalf("missing partial-save explanation: %s", out.String())
	}
	ov, err := codexsettings.LoadOverlay()
	if err != nil || ov["model_verbosity"] != "low" {
		t.Fatalf("saved preference unavailable after apply failure: %v, %v", ov, err)
	}
	out.Reset()
	if rc := Run(Options{Home: home, In: strings.NewReader(""), Out: out}, "codex"); rc != 1 {
		t.Fatalf("native read failure rc=%d want=1: %s", rc, out.String())
	}
}

func TestCodexSettingsCheckDetectsUnappliedReset(t *testing.T) {
	home, path := codexSettingsFixture(t)
	if err := codexsettings.SaveOverlay(map[string]string{"model_verbosity": "low"}); err != nil {
		t.Fatal(err)
	}
	if err := generate.ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	// A failed native reset leaves the new empty overlay saved and the old owned preference on disk.
	if err := codexsettings.SaveOverlay(map[string]string{}); err != nil {
		t.Fatal(err)
	}
	nativeBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	vaultBefore, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	o := Options{Home: home, Check: true, In: strings.NewReader(""), Out: &out}
	if rc := Run(o, "codex"); rc != 1 {
		t.Fatalf("unapplied reset rc=%d want=1: %s", rc, out.String())
	}
	nativeAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	vaultAfter, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(nativeBefore, nativeAfter) || !bytes.Equal(vaultBefore, vaultAfter) {
		t.Fatal("check of unapplied reset changed native config or vault")
	}
	if err := generate.ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if rc := Run(o, "codex"); rc != 0 {
		t.Fatalf("applied reset rc=%d want=0: %s", rc, out.String())
	}
}

func TestWebSearchCodexHomeNoPlaintextBackup(t *testing.T) {
	cfgDir := vaulttest.Use(t) // the backup is vault-sealed; a hermetic key keeps CI off the OS keyring
	home, codexHome := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	custom := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(custom, []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reg func(string) error
	for _, c := range webSearchClients() {
		if c.name == "codex" {
			reg = c.register
		}
	}
	if reg == nil {
		t.Fatal("no codex client")
	}
	if err := reg("/bin/claude-code"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(custom)
	if !strings.Contains(string(got), "[mcp_servers.web_search]") {
		t.Errorf("$CODEX_HOME/config.toml not edited: %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); err == nil {
		t.Error("~/.codex/config.toml was written despite CODEX_HOME")
	}
	var leftovers []string
	filepath.WalkDir(cfgDir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() && strings.Contains(filepath.Base(p), "config.toml") {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	entries, _ := os.ReadDir(codexHome)
	for _, e := range entries {
		if e.Name() != "config.toml" {
			leftovers = append(leftovers, e.Name())
		}
	}
	if len(leftovers) > 0 {
		t.Errorf("plaintext backup left behind: %v", leftovers)
	}
}
