package agent_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestConfigHistoryIsEncryptedBoundedAndSurvivesRotation(t *testing.T) {
	dir, _ := vaulttest.UseKeyring(t)
	path := filepath.Join(dir, "native", "config.toml")
	other := filepath.Join(dir, "other", "config.toml")
	fixture := []byte("private-config-fixture-68b19d\nforeign = 'preserve exactly'\n")
	if err := agent.SaveCodexBaseline(path, fixture); err != nil {
		t.Fatal(err)
	}
	for i := range 7 {
		if err := agent.CodexBackup(path, fmt.Appendf(nil, "version %d", i), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := agent.CodexBackup(other, fixture, false); err != nil {
		t.Fatal(err)
	}
	vaulttest.Secret(t, "API_KEY", "credential-fixture")
	pairs, err := secrets.ExportPairs(nil)
	if err != nil || !reflect.DeepEqual(pairs, []secrets.Pair{{Name: "API_KEY", Value: "credential-fixture"}}) {
		t.Fatalf("credential export includes unexpected entries: %v, %v", pairs, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state", "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, clear := range [][]byte{fixture, []byte(base64.StdEncoding.EncodeToString(fixture)), []byte("private-config-fixture-68b19d")} {
		if bytes.Contains(raw, clear) {
			t.Fatal("vault contains plaintext config contents")
		}
	}
	oldKid, newKid, err := vault.RotateKey()
	if err != nil || oldKid == newKid {
		t.Fatalf("rotation did not change the key: %v", err)
	}
	baseline, err := agent.LoadCodexBaseline(path)
	if err != nil || !bytes.Equal(baseline, fixture) {
		t.Fatalf("baseline did not survive rotation: %v", err)
	}
	pairs, err = secrets.ExportPairs(nil)
	if err != nil || !reflect.DeepEqual(pairs, []secrets.Pair{{Name: "API_KEY", Value: "credential-fixture"}}) {
		t.Fatalf("credential did not survive rotation: %v", err)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		for _, name := range tx.Names(vault.SettingsBackups) {
			r, found, err := tx.RecordIn(vault.SettingsBackups, name)
			if err != nil || !found || r.Kid != newKid || r.Prefix != "" {
				t.Errorf("backup metadata after rotation: found=%v, kid=%q, prefix=%q, err=%v", found, r.Kid, r.Prefix, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 6; i >= 2; i-- {
		b, found, err := agent.LatestCodexBackup(path)
		if err != nil || !found || string(b.Data) != fmt.Sprintf("version %d", i) || !b.Existed || b.Path != path {
			t.Fatalf("latest backup %d: found=%v, data=%q, err=%v", i, found, b.Data, err)
		}
		if err := agent.ConsumeCodexBackup(b.Name); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := agent.LatestCodexBackup(path); err != nil || found {
		t.Fatalf("more than five snapshots retained: found=%v, err=%v", found, err)
	}
	b, found, err := agent.LatestCodexBackup(other)
	if err != nil || !found || !bytes.Equal(b.Data, fixture) || b.Existed || b.Path != other {
		t.Fatalf("other path snapshot changed: found=%v, err=%v", found, err)
	}
	baseline, err = agent.LoadCodexBaseline(path)
	if err != nil || !bytes.Equal(baseline, fixture) {
		t.Fatalf("snapshot consumption changed baseline: %v", err)
	}
}

func TestCredentialAndConfigCiphertextCannotBeExchanged(t *testing.T) {
	vaulttest.UseKeyring(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := agent.CodexBackup(path, []byte("original native config"), true); err != nil {
		t.Fatal(err)
	}
	b, found, err := agent.LatestCodexBackup(path)
	if err != nil || !found {
		t.Fatalf("backup missing: %v", err)
	}
	vaulttest.Secret(t, b.Name, "credential using identical record name")
	if err := vault.Update(func(tx *vault.Tx) error {
		credential, _, err := tx.Record(b.Name)
		if err != nil {
			return err
		}
		backup, _, err := tx.RecordIn(vault.SettingsBackups, b.Name)
		if err != nil {
			return err
		}
		if err := tx.PutRecord(b.Name, backup); err != nil {
			return err
		}
		return tx.PutRecordIn(vault.SettingsBackups, b.Name, credential)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := vault.ReadSecret(b.Name); err == nil {
		t.Fatal("credential collection decrypted settings ciphertext")
	}
	if _, _, err := vault.ReadSettingsBackup(b.Name); err == nil {
		t.Fatal("settings collection decrypted credential ciphertext")
	}
}

func TestPreferencesRejectInvalidEditsWithoutOverwritingSavedValues(t *testing.T) {
	vaulttest.Use(t)
	want := map[string]string{"model_reasoning_effort": "high", "model_verbosity": "low"}
	if err := agent.SaveCodexOverlay(want); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []map[string]string{
		{"model_reasoning_effort": "unbounded"},
		{"model_verbosity": "verbose"},
		{"max_output_tokens": "256"},
	} {
		if err := agent.SaveCodexOverlay(invalid); err == nil {
			t.Fatalf("accepted invalid preferences: %v", invalid)
		}
		got, err := agent.LoadCodexOverlay()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("invalid edit overwrote saved preferences: %v, %v", got, err)
		}
	}
	if err := agent.UpdateCodexOverlay(func(current map[string]string) map[string]string {
		current["model_verbosity"] = "medium"
		return current
	}); err != nil {
		t.Fatal(err)
	}
	want["model_verbosity"] = "medium"
	got, err := agent.LoadCodexOverlay()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("staged preference merge lost an untouched preference: %v, %v", got, err)
	}
}

func TestConfigPathPrecedence(t *testing.T) {
	home, native := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", native)
	for _, tc := range []struct{ home, want string }{
		{home, filepath.Join(home, ".codex", "config.toml")},
		{"", filepath.Join(native, "config.toml")},
	} {
		got, err := agent.CodexConfigPath(tc.home)
		if err != nil || got != tc.want {
			t.Fatalf("ConfigPath(%q) = %q, %v; want %q", tc.home, got, err, tc.want)
		}
	}
	t.Setenv("CODEX_HOME", "")
	defaultHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := agent.CodexConfigPath(""); err != nil || got != filepath.Join(defaultHome, ".codex", "config.toml") {
		t.Fatalf("default ConfigPath = %q, %v", got, err)
	}
}

func TestCodexLifecycleTrustUsesMergedPositions(t *testing.T) {
	home := scratch(t)
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(dir, "hooks.json")
	seed := []byte(`{"hooks":{
		"SessionStart":[{"hooks":[{"type":"command","command":"orca-session"}]},{"hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook stale"}]}],
		"SubagentStart":[{"hooks":[{"type":"command","command":"orca-child"}]},{"hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook stale"}]}]
	}}`)
	if err := os.WriteFile(hooksPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.RenderConfigs(agent.RenderOptions{Home: home, ForceHomes: true, Agents: []string{"codex"}, Out: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ event, native, command, foreign string }{
		{"SessionStart", "session_start", "SessionStart --codex-routing-start", "orca-session"},
		{"SubagentStart", "subagent_start", "SubagentStart --codex-subagent-start", "orca-child"},
	} {
		groups := codexGroupOrder(t, merged, c.event)
		if len(groups) != 2 || !strings.Contains(string(groups[0]), c.foreign) || !strings.Contains(string(groups[1]), "hook "+c.command) {
			t.Fatalf("%s merge lost lifecycle ordering: %s", c.event, merged)
		}
		// Canonical identities observed in Codex 0.163.0, independent of the production digest helper.
		identity := fmt.Sprintf(`{"event_name":%q,"hooks":[{"async":false,"command":"HARNESS_HOOK_AGENT=codex $HOME/.claude/bin/claude-code hook %s","timeout":5,"type":"command"}]}`, c.native, c.command)
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(identity)))
		source := agent.CodexHookSourcePath(hooksPath)
		want := fmt.Sprintf("[hooks.state.%q]\ntrusted_hash = %q", source+":"+c.native+":1:0", digest)
		if !strings.Contains(string(config), want) {
			t.Errorf("missing lifecycle trust %s in %s", want, config)
		}
		if strings.Contains(string(config), source+":"+c.native+":0:0") {
			t.Errorf("foreign lifecycle hook was trusted: %s", config)
		}
	}
	unsupported := []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook stop"}]}]}}`)
	if _, err := agent.CodexTrustEntries(hooksPath, unsupported); err == nil || !strings.Contains(err.Error(), `unsupported hook event "Stop"`) {
		t.Errorf("unsupported harness event error = %v", err)
	}
}

func TestCodexHomeControlsConfigAndHooks(t *testing.T) {
	for _, explicitHome := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit_home_%v", explicitHome), func(t *testing.T) {
			home := scratch(t)
			t.Setenv("HOME", home)
			codexHome := filepath.Join(t.TempDir(), "custom-codex")
			t.Setenv("CODEX_HOME", codexHome)
			o := agent.RenderOptions{ForceHomes: true, Agents: []string{"codex"}, Out: &bytes.Buffer{}}
			want, absent := codexHome, filepath.Join(home, ".codex")
			if explicitHome {
				o.Home = home
				want, absent = absent, want
			}
			if _, err := agent.RenderConfigs(o); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"config.toml", "hooks.json"} {
				if _, err := os.Stat(filepath.Join(want, name)); err != nil {
					t.Errorf("missing %s in selected home: %v", name, err)
				}
				if _, err := os.Stat(filepath.Join(absent, name)); !os.IsNotExist(err) {
					t.Errorf("wrote %s in unselected home: %v", name, err)
				}
			}
			config, err := os.ReadFile(filepath.Join(want, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(config), agent.CodexHookSourcePath(filepath.Join(want, "hooks.json"))+":session_start:0:0") {
				t.Errorf("trust source does not follow selected home: %s", config)
			}
		})
	}
}

// agent.MergeCodexTrust must not mistake a "["-led line inside an open array for a table header: an
// existing trusted_hash AFTER the array, still inside the same table, is replaced in place —
// not left stale with a duplicate appended at EOF because the array wrongly reset the active table.
func TestMergeCodexTrustBracketInArrayNotHeader(t *testing.T) {
	raw := "[hooks.state.\"K\"]\nextra = [\n  [1, 2],\n]\ntrusted_hash = \"old\"\n"
	entries := map[string]string{"K": "abc123"}
	got := agent.MergeCodexTrust(raw, entries)
	want := "[hooks.state.\"K\"]\nextra = [\n  [1, 2],\n]\ntrusted_hash = \"abc123\"\n"
	if got != want {
		t.Fatalf("mergeCodexTrust:\n%q\nwant:\n%q", got, want)
	}
	if strings.Count(got, "trusted_hash") != 1 {
		t.Errorf("want exactly one trusted_hash line, got:\n%q", got)
	}
}

// A real table header following a "["-led array element must still pick up its own hash, proving
// the depth tracking resets once the array closes rather than staying stuck open.
func TestMergeCodexTrustHeaderAfterArrayStillMatches(t *testing.T) {
	raw := "extra = [\n  [1, 2],\n]\n[hooks.state.\"K2\"]\nother = 1\n"
	entries := map[string]string{"K2": "def456"}
	got := agent.MergeCodexTrust(raw, entries)
	want := "extra = [\n  [1, 2],\n]\n[hooks.state.\"K2\"]\nother = 1\ntrusted_hash = \"def456\"\n"
	if got != want {
		t.Fatalf("mergeCodexTrust:\n%q\nwant:\n%q", got, want)
	}
}

func TestMergeCodexTrustPreservesUnownedText(t *testing.T) {
	raw := `note = """
[hooks.state."active"]
trusted_hash = "inside string"
"""
[hooks.state.'active'] # keep table comment
  'trusted_hash' = 'old' # keep hash comment
other = 1
[hooks.state."missing"]
other = 2
[foreign]
trusted_hash = "unowned"`
	want := `note = """
[hooks.state."active"]
trusted_hash = "inside string"
"""
[hooks.state.'active'] # keep table comment
  'trusted_hash' = "new" # keep hash comment
other = 1
[hooks.state."missing"]
other = 2
trusted_hash = "added"
[foreign]
trusted_hash = "unowned"

[hooks.state."a"]
trusted_hash = "first"

[hooks.state."z"]
trusted_hash = "last"
`
	entries := map[string]string{"active": "new", "missing": "added", "z": "last", "a": "first"}
	got := agent.MergeCodexTrust(raw, entries)
	if got != want {
		t.Fatalf("mergeCodexTrust:\n%s\nwant:\n%s", got, want)
	}
	if again := agent.MergeCodexTrust(got, entries); again != got {
		t.Fatalf("merge is not idempotent:\n%s", again)
	}
	if len(entries) != 4 {
		t.Fatal("merge mutated input entries")
	}
}

// TestInsertTopLevelTOMLSkipsMultilineStrings: a "["-led line inside a TOML multiline string
// (triple-quoted) must not be mistaken for the first top-level table header.
func TestInsertTopLevelTOMLSkipsMultilineStrings(t *testing.T) {
	cases := []struct {
		name  string
		quote string
	}{
		{"doubleQuoted", `"""`},
		{"singleQuoted", `'''`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := "note = " + c.quote + "\nsome text\n[looks like a table]\nmore text\n" + c.quote + "\n\n[real.table]\nkey = 1\n"
			section := "[inserted]\nx = 1\n"
			got := agent.InsertTopLevelTOML(in, section)

			wantString := "note = " + c.quote + "\nsome text\n[looks like a table]\nmore text\n" + c.quote + "\n"
			idxString := indexOf(got, wantString)
			idxInserted := indexOf(got, "[inserted]")
			idxRealTable := indexOf(got, "[real.table]")

			if idxString == -1 {
				t.Fatalf("multiline string content not byte-for-byte unchanged, got:\n%s", got)
			}
			if idxInserted == -1 || idxRealTable == -1 {
				t.Fatalf("missing expected sections, got:\n%s", got)
			}
			if idxString >= idxInserted || idxInserted >= idxRealTable {
				t.Errorf("want order: multiline string, then [inserted], then [real.table]; got:\n%s", got)
			}
		})
	}
}

// TestInsertTopLevelTOMLSkipsNestedArrays: a "["-led line inside an open multiline array (nested
// array element, not a multiline string) must not be mistaken for the first top-level table header.
func TestInsertTopLevelTOMLSkipsNestedArrays(t *testing.T) {
	in := "matrix = [\n  [1, 2],\n  [3, 4],\n]\n\n[real]\nk = 1\n"
	section := "[inserted]\nx = 1\n"
	got := agent.InsertTopLevelTOML(in, section)

	wantArray := "matrix = [\n  [1, 2],\n  [3, 4],\n]\n"
	idxArray := indexOf(got, wantArray)
	idxInserted := indexOf(got, "[inserted]")
	idxReal := indexOf(got, "[real]")

	if idxArray == -1 {
		t.Fatalf("array bytes not byte-for-byte unchanged, got:\n%s", got)
	}
	if idxInserted == -1 || idxReal == -1 {
		t.Fatalf("missing expected sections, got:\n%s", got)
	}
	if idxArray >= idxInserted || idxInserted >= idxReal {
		t.Errorf("want order: array, then [inserted], then [real]; got:\n%s", got)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
