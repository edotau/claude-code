package generate

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/settings/codexsettings"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCodexPreferencesApplyPreservesAndRestoresNativeConfig(t *testing.T) {
	vaulttest.Use(t)
	native := t.TempDir()
	t.Setenv("CODEX_HOME", native)
	path, target := filepath.Join(native, "config.toml"), filepath.Join(t.TempDir(), "tracked.toml")
	multiline := "instructions = '''\n" + codexSettingsBegin + "\nmodel_verbosity = 'high'\n" + codexSettingsEnd + "\n[not_a_table]\n'''\n"
	foreign := "# foreign preferences\nfixture_secret = 'private-fixture-config'\n" + multiline
	table := "[model_providers.foreign]\nname = 'my provider'\nbase_url = 'http://127.0.0.1:1'\nmodel_verbosity = 'high'\n"
	original := []byte("'model_verbosity' = 'high' # chosen before harness\n" + foreign + table)
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := codexsettings.SaveOverlay(map[string]string{"model_verbosity": "low", "model_reasoning_effort": "medium"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodexSettings(""); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), foreign) || !strings.Contains(string(after), table) {
		t.Fatal("foreign text changed")
	}
	if strings.Contains(string(after), "chosen before harness") {
		t.Fatal("duplicate root setting was kept")
	}
	values, err := ReadCodexSettings("")
	if err != nil || values["model_verbosity"] != "low" || values["model_reasoning_effort"] != "medium" {
		t.Fatalf("preferences not applied: %v, %v", values, err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("outgoing symlink replaced: %v", err)
	}
	b, found, err := codexsettings.LatestBackup(path)
	if err != nil || !found || !bytes.Equal(b.Data, original) || !b.Existed {
		t.Fatalf("original snapshot missing: found=%v err=%v", found, err)
	}
	baseline, err := codexsettings.LoadBaseline(path)
	if err != nil || !bytes.Equal(baseline, after) {
		t.Fatalf("encrypted baseline missing: %v", err)
	}
	for _, p := range []string{path + ".backup", vendorBaselinePath("codex", path)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("unexpected plaintext history at %s: %v", p, err)
		}
	}
	if err := ApplyCodexSettings(""); err != nil {
		t.Fatal(err)
	}
	latest, found, err := codexsettings.LatestBackup(path)
	if err != nil || !found || latest.Name != b.Name {
		t.Fatal("no-op apply added a snapshot")
	}
	if err := Revert(Options{Agents: []string{"codex"}, DryRun: true, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(unchanged, after) {
		t.Fatalf("dry restore modified config: %v", err)
	}
	if err := Revert(Options{Agents: []string{"codex"}, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatalf("restore did not recover exact original: %v", err)
	}
	if err := ApplyCodexSettings(""); err != nil {
		t.Fatal(err)
	}
	restored, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatalf("saved preferences undid restore: %v", err)
	}
}

func TestCodexPreferencesRejectUnsafeWrites(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("model_verbosity = 'high'\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := codexsettings.SaveOverlay(map[string]string{"model_verbosity": "low"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	if err := ApplyCodexSettings(home); err == nil || !strings.Contains(err.Error(), "encrypted backup failed") {
		t.Fatalf("write must fail without encrypted backup: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("backup failure overwrote config: %v", err)
	}
	if err := writeCodexConfig(path, []byte("stale file"), []byte("replacement"), true); err == nil || !strings.Contains(err.Error(), "changed before write") {
		t.Fatalf("stale write was not rejected before backup: %v", err)
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("stale write overwrote config: %v", err)
	}
}

func TestCodexPreferencesRestoreMissingOriginalAndOwnedValues(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := codexsettings.SaveOverlay(map[string]string{"model_verbosity": "low"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	if err := codexsettings.SaveOverlay(map[string]string{"model_verbosity": "high"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	opts := Options{Home: home, Agents: []string{"codex"}, Out: io.Discard}
	if err := Revert(opts); err != nil {
		t.Fatal(err)
	}
	ov, err := codexsettings.LoadOverlay()
	if err != nil || ov["model_verbosity"] != "low" {
		t.Fatalf("restored preference was not synchronized: %v, %v", ov, err)
	}
	if err := Revert(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("restore should remove originally absent config: %v", err)
	}
}

func TestCodexPreferenceParserRejectsTrailingGarbage(t *testing.T) {
	for _, value := range []string{`"low" trailing`, `'low' trailing`, "`low`"} {
		if _, err := readCodexSettings("model_verbosity = " + value); err == nil {
			t.Fatalf("accepted unsupported string syntax %s", value)
		}
	}
	for _, value := range []string{`"low" # comment`, `'low' # comment`} {
		got, err := readCodexSettings("model_verbosity = " + value)
		if err != nil || got["model_verbosity"] != "low" {
			t.Fatalf("valid scalar rejected: %v", err)
		}
	}
}

func TestCodexPreferencesNoopWithoutTables(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model = 'fixture'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := codexsettings.SaveOverlay(map[string]string{"model_verbosity": "low"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b, found, err := codexsettings.LatestBackup(path)
	if err != nil || !found {
		t.Fatalf("snapshot missing: %v", err)
	}
	if err := ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("no-op apply changed config: %v", err)
	}
	next, found, err := codexsettings.LatestBackup(path)
	if err != nil || !found || next.Name != b.Name {
		t.Fatal("no-op apply added a snapshot")
	}
	if drift, err := CodexSettingsDrift(home); err != nil || drift {
		t.Fatalf("applied preferences still report drift: %v", err)
	}
}

func TestCodexPreferencesQuotedKeyIdentity(t *testing.T) {
	foreign := "'\"model_verbosity\"' = 'foreign'\n"
	raw := foreign + "\"model\\u005fverbosity\" = 'high'\n"
	live, err := readCodexSettings(raw)
	if err != nil || live["model_verbosity"] != "high" {
		t.Fatalf("escaped native key was not decoded: %v, %v", live, err)
	}
	next, err := patchCodexSettings(raw, map[string]string{"model_verbosity": "low"})
	if err != nil || !strings.Contains(next, foreign) || strings.Contains(next, "'high'") {
		t.Fatalf("quoted key identity was lost: %v", err)
	}
}

func TestCodexRestoreMissingOriginalKeepsSymlinkTarget(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := codexsettings.SaveOverlay(map[string]string{"model_verbosity": "low"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "tracked.toml")
	if err := os.WriteFile(target, []byte("model = 'keep'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := Revert(Options{Home: home, Agents: []string{"codex"}, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "model = 'keep'\n" {
		t.Fatalf("restore deleted the symlink target: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("restore left the config link behind: %v", err)
	}
}

func TestCodexLockIsSharedAcrossSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.toml")
	a, b := filepath.Join(dir, "a.toml"), filepath.Join(dir, "b.toml")
	if err := os.WriteFile(real, []byte("x = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{a, b} {
		if err := os.Symlink(real, l); err != nil {
			t.Fatal(err)
		}
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = withCodexLock(a, func() error { close(held); <-release; return nil })
	}()
	<-held
	go func() {
		_ = withCodexLock(b, func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("second symlink took a different lock while the first was held")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second symlink never acquired the lock")
	}
}

func TestCodexPreferenceParserAcceptsMultilineStrings(t *testing.T) {
	for _, value := range []string{"'''low'''", `"""low"""`, "'''\nlow'''", "\"\"\"\nlow\n\"\"\" # c"} {
		raw := "model_verbosity = " + value + "\nmodel = 'x'\n"
		got, err := readCodexSettings(raw)
		want := "low"
		if strings.Contains(value, "\nlow\n") {
			want = "low\n"
		}
		if err != nil || got["model_verbosity"] != want {
			t.Fatalf("%q: got %q, %v", value, got["model_verbosity"], err)
		}
	}
	next, err := patchCodexSettings("model_verbosity = '''high'''\n", map[string]string{"model_verbosity": "low"})
	if err != nil || strings.Contains(next, "high") || !strings.Contains(next, `model_verbosity = "low"`) {
		t.Fatalf("multiline native value not replaced in existing style: %q, %v", next, err)
	}
}
