package setup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// decodeObj decodes a JSON object into key->raw, for presence/absence and value assertions.
func decodeObj(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

// rawEqual compares two JSON values by parsed structure, not byte layout.
func rawEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return reflect.DeepEqual(av, bv)
}

// TestStripWindowsOnly proves stripWindowsOnly drops hooks/statusLine and every Linux-path string
// (top-level and inside env), while keeping plain keys and permissions untouched.
func TestStripWindowsOnly(t *testing.T) {
	in := []byte(`{
		"theme": "auto",
		"statusLine": {"type": "command", "command": "status-script"},
		"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "foreign stop"}]}]},
		"permissions": {"allow": ["Bash(ls:*)"]},
		"SOME_PATH_KEY": "/home/tester/.claude/skills",
		"env": {
			"HARNESS_BIN": "/usr/local/bin/claude-code",
			"HOME_REF": "$HOME/.claude/bin",
			"TILDE_REF": "~/.claude/bin/claude-code",
			"PLAIN_VAR": "hello"
		}
	}`)
	out, err := stripWindowsOnly(in)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeObj(t, out)
	for _, k := range []string{"statusLine", "hooks", "SOME_PATH_KEY"} {
		if _, ok := m[k]; ok {
			t.Errorf("stripWindowsOnly kept %q, want dropped", k)
		}
	}
	if v, ok := m["theme"]; !ok || !rawEqual(t, v, json.RawMessage(`"auto"`)) {
		t.Errorf("theme = %s, want kept as \"auto\"", v)
	}
	assertWindowsPermissions(t, m)
	assertWindowsEnvironment(t, m)
}

func assertWindowsEnvironment(t *testing.T, m map[string]json.RawMessage) {
	t.Helper()
	envRaw, ok := m["env"]
	if !ok {
		t.Fatal("env dropped, want kept (filtered)")
	}
	env := decodeObj(t, envRaw)
	for _, k := range []string{"HARNESS_BIN", "HOME_REF", "TILDE_REF"} {
		if _, ok := env[k]; ok {
			t.Errorf("env.%s kept, want dropped as a Linux path", k)
		}
	}
	if v, ok := env["PLAIN_VAR"]; !ok || !rawEqual(t, v, json.RawMessage(`"hello"`)) {
		t.Errorf("env.PLAIN_VAR = %s, want kept as \"hello\"", v)
	}
}

func assertWindowsPermissions(t *testing.T, m map[string]json.RawMessage) {
	t.Helper()
	perms, ok := m["permissions"]
	if !ok {
		t.Fatal("permissions dropped, want kept")
	}
	if want := []byte(`{"allow": ["Bash(ls:*)"]}`); !rawEqual(t, perms, want) {
		t.Errorf("permissions = %s, want %s", perms, want)
	}
}

// TestStripWindowsOnlyKeepsNonPathStrings proves the marker match is substring-specific: a plain
// string that merely looks unix-ish (no marker substring) survives, so the strip isn't over-eager.
func TestStripWindowsOnlyKeepsNonPathStrings(t *testing.T) {
	in := []byte(`{"NOTE": "/opt/foo/bar", "COUNT": "3"}`)
	out, err := stripWindowsOnly(in)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeObj(t, out)
	if v, ok := m["NOTE"]; !ok || !rawEqual(t, v, json.RawMessage(`"/opt/foo/bar"`)) {
		t.Errorf("NOTE = %s, want kept (no marker matched)", v)
	}
	if _, ok := m["COUNT"]; !ok {
		t.Error("COUNT dropped, want kept")
	}
}

// winHermetic isolates config/vault (hermetic) and points the Windows profile at a fresh temp dir,
// so these tests never touch the real Windows profile.
func winHermetic(t *testing.T) string {
	t.Helper()
	hermetic(t)
	profile := t.TempDir()
	t.Setenv(windowsProfileEnv, profile)
	return profile
}

func writeWindowsFixture(t *testing.T, file string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func winSettingsPath(profile string) string {
	return filepath.Join(profile, ".claude", "settings.json")
}

// TestInstallWindowsSettingsSeedsMissingKeepsExisting: a missing file gets the render minus hooks and statusLine;
// an existing one (BOM included) stays byte-identical.
func TestInstallWindowsSettingsSeedsMissingKeepsExisting(t *testing.T) {
	profile := winHermetic(t)
	file := winSettingsPath(profile)
	if err := installWindowsSettings(&bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeObj(t, b)
	if _, ok := m["hooks"]; ok || m["$schema"] == nil {
		t.Errorf("seeded Windows settings.json = %s, want the render without hooks", b)
	}
	existing := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"theme":"light"}`)...)
	writeWindowsFixture(t, file, existing)
	var out bytes.Buffer
	if err := installWindowsSettings(&out, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(file); !bytes.Equal(got, existing) || !strings.Contains(out.String(), "kept (yours)") {
		t.Errorf("existing Windows settings.json rewritten: %q (%q)", got, out.String())
	}
}

// brokenCmdPATH points PATH at an empty dir so `cmd.exe` can never be found, without touching the
// real Windows profile or filesystem; it leaves HARNESS_WINDOWS_PROFILE unset so resolution falls
// through to the real cmd.exe lookup, which then fails safely.
func brokenCmdPATH(t *testing.T) {
	t.Helper()
	t.Setenv(windowsProfileEnv, "")
	t.Setenv("PATH", t.TempDir())
}

// TestInstallWindowsSettings_MissingProfileSkip proves installWindowsSettings prints a one-line skip
// note and returns nil, never an error, when the profile can't be resolved.
func TestInstallWindowsSettings_MissingProfileSkip(t *testing.T) {
	hermetic(t)
	setOnWSL(t, true)
	brokenCmdPATH(t)
	var out bytes.Buffer
	if err := installWindowsSettings(&out, false); err != nil {
		t.Fatalf("err = %v, want nil (skip, not failure)", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("skip")) {
		t.Errorf("output %q missing a skip note", out.String())
	}
}

// setOnWSL swaps the onWSL seam for the test.
func setOnWSL(t *testing.T, v bool) {
	t.Helper()
	orig := onWSL
	onWSL = func() bool { return v }
	t.Cleanup(func() { onWSL = orig })
}

// TestInstallWindowsSettings_NonWSLNoop proves installWindowsSettings is silent off WSL.
func TestInstallWindowsSettings_NonWSLNoop(t *testing.T) {
	hermetic(t)
	t.Setenv(windowsProfileEnv, "")
	setOnWSL(t, false)
	var out bytes.Buffer
	if err := installWindowsSettings(&out, false); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("non-WSL must no-op silently, got %q", out.String())
	}
}
