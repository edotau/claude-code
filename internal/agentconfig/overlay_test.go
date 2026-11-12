package agentconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Each tampered or tricked overlay document is refused with its own message; an own 0600 file loads.
func TestLoadOverlayRefusesWritableOrTrickedFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, doc string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(doc), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil { // WriteFile's mode is masked by umask
			t.Fatal(err)
		}
		return p
	}
	own := write("own.json", `{"provider":"databricks","profile":"dev"}`, 0o600)
	big := `{"model":"` + strings.Repeat("a", overlayMaxBytes) + `"}`
	cases := map[string]string{
		`{"schema":2}`:      "schema 2 is newer than this build",
		`{} {}`:             "not valid JSON at byte 4", // schemas.Decode refuses it before the trailing-data check
		big:                 "over 65536 bytes",
		`{"files":[]}`:      "files: not supported yet",
		`{"profile":"dev"}`: "profile: needs a provider",
		`{"provider":"databricks","profile":"d\u0007ev"}`: "profile: blank or holds a control character",
	}
	if runtime.GOOS != "windows" {
		cases[write("loose.json", `{"host":"evil"}`, 0o666)] = "group- or world-writable"
		fifo := filepath.Join(dir, "fifo")
		if err := exec.Command("mkfifo", fifo).Run(); err != nil {
			t.Fatal(err)
		}
		cases[fifo] = "not a regular file"
	}
	for doc, want := range cases {
		if _, err := LoadOverlay(doc, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadOverlay(%.40q): err = %v, want %q", doc, err, want)
		} else if strings.Contains(err.Error(), "\a") || strings.Contains(err.Error(), "evil") {
			t.Errorf("LoadOverlay(%.40q): err %q echoes a value", doc, err)
		}
	}
	if o, err := LoadOverlay(own, nil); err != nil || o.Confinement() != "databricks:dev" {
		t.Errorf("own 0600 file: overlay %+v, err %v; want databricks:dev", o, err)
	}
}

// Harness-owned agent config names are refused by --env and --keep-env; an ordinary name passes.
func TestFlagOverlayRefusesHarnessOwnedNames(t *testing.T) {
	for _, name := range []string{"OPENCODE_PERMISSION", "OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "OPENCODE_CONFIG_DIR",
		"OPENCODE_DISABLE_MODELS_FETCH", "CODEX_HOME", "COPILOT_HOME", "COPILOT_ALLOW_ALL", "COPILOT_ASSISTED_APPROVAL",
		"COPILOT_CUSTOM_INSTRUCTIONS_DIRS", "HARNESS_FOO"} {
		if _, err := FlagOverlay([]string{name + "=x"}, nil); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("--env %s: err = %v, want a refusal naming it", name, err)
		}
		if _, err := FlagOverlay(nil, []string{name}); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("--keep-env %s: err = %v, want a refusal naming it", name, err)
		}
	}
	if _, err := FlagOverlay([]string{"FOO=bar"}, []string{"BAR"}); err != nil {
		t.Errorf("FOO=bar: %v", err)
	}
}

// A @secret ref naming a profile's route credential or a harness-internal secret is refused, naming the ref.
func TestRefRuleRefusesRouteCredentialAndInternalRefs(t *testing.T) {
	setup(t)
	for doc, want := range map[string]string{
		`{"set":{"MY_PAT":"@secret:DATABRICKS_DEV_PAT"}}`: "@secret:DATABRICKS_DEV_PAT is a profile's route credential: use --profile",
		`{"set":{"A":"@secret:HARNESS_VAULT_KEY"}}`:       "@secret:HARNESS_VAULT_KEY is harness-internal",
	} {
		if _, err := LoadOverlay(doc, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadOverlay(%s): err = %v, want %q", doc, err, want)
		}
	}
	if _, err := LoadOverlay(`{"set":{"A":"@secret:MY_OWN"}}`, nil); err != nil {
		t.Errorf("an ordinary ref: %v", err)
	}
}
