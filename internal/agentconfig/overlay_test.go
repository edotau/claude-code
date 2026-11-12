package agentconfig

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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

// An unset, keep or set entry naming a route endpoint or proxy is refused with the route-endpoint message, value unechoed.
func TestOverlayUnsetKeepRefuseRouteEndpoints(t *testing.T) {
	for _, doc := range []string{`{"unset":["ANTHROPIC_BASE_URL"]}`, `{"unset":["HTTPS_PROXY"]}`,
		`{"keep":["OPENAI_BASE_URL"]}`, `{"set":{"AGY_LLM_GATEWAY_URL":"https://evil.example"}}`} {
		_, err := LoadOverlay(doc, nil)
		if !errors.Is(err, errBaseURL) {
			t.Errorf("LoadOverlay(%s): err = %v, want %q", doc, err, errBaseURL)
		} else if strings.Contains(err.Error(), "evil") {
			t.Errorf("LoadOverlay(%s): err %q echoes the value", doc, err)
		}
	}
}

// ResolveOverlay fails closed naming the set key and ref (no value, no key-tool stderr); success swaps and clones.
func TestResolveOverlayFailsClosedAndNeverEchoes(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "MY_OWN", "s3cr3t-value")
	in := func() Overlay {
		return Overlay{Set: map[string]string{"K": "@secret:MY_OWN", "PLAIN": "x"}, Unset: []string{"A"}, Keep: []string{"B"}}
	}
	o := in()
	r, err := ResolveOverlay(o)
	want := Resolved{Set: map[string]string{"K": "s3cr3t-value", "PLAIN": "x"}, Unset: []string{"A"}, Keep: []string{"B"}}
	if err != nil || !reflect.DeepEqual(r, want) {
		t.Fatalf("ResolveOverlay = %+v, %v; want %+v", r, err, want)
	}
	r.Unset[0], r.Keep[0] = "X", "X"
	if !reflect.DeepEqual(o, in()) {
		t.Errorf("ResolveOverlay shares or mutates its input: %+v", o)
	}
	cases := []struct {
		o    Overlay
		arm  func()
		want string
	}{
		{Overlay{Set: map[string]string{"K": "@secret:MISSING"}}, func() {}, "overlay set K: @secret:MISSING is not in the vault"},
		{in(), func() { // a wrong env key falls through to key_command, which fails loudly on stderr
			k, err := vault.EnsureKey()
			if err != nil {
				t.Fatal(err)
			}
			vaulttest.Keyring(t)
			vaulttest.Put(t, vault.Meta, "kid", k.Kid) // a recorded kid sends a mismatched key down the fallback chain
			vaulttest.Put(t, vault.Meta, "key_command", `["sh","-c","echo LEAKED-STDERR >&2; exit 3"]`)
			t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
		}, "overlay set K: @secret:MY_OWN: " + vault.ErrWrongKey.Error()},
	}
	if runtime.GOOS == "windows" {
		cases = cases[:1] // no sh for the failing key_command
	}
	leak := regexp.MustCompile(`LEAKED|s3cr3t`)
	for _, c := range cases {
		c.arm()
		if _, err := ResolveOverlay(c.o); err == nil || !strings.HasPrefix(err.Error(), c.want) || leak.MatchString(err.Error()) {
			t.Errorf("ResolveOverlay(%v): err = %v, want prefix %q and no value or key-tool stderr", c.o.Set, err, c.want)
		}
	}
}

// A stored overlay applies only when the flag and process env are empty and the dispatch is not nested.
func TestDispatchOverlayPrecedence(t *testing.T) {
	vaulttest.Use(t)
	stored := `{"provider":"databricks","profile":"auto"}`
	vaulttest.Put(t, vault.Env, OverlayVar, stored)
	for _, tc := range []struct{ name, flag, env, confined, want string }{
		{"stored alone", "", "", "", stored},
		{"process env wins", "", `{"provider":"openai"}`, "", `{"provider":"openai"}`},
		{"flag wins", `{"provider":"github"}`, `{"provider":"openai"}`, "", `{"provider":"github"}`},
		{"nested ignores stored", "", "", "databricks:qa", ""},
	} {
		t.Setenv(OverlayVar, tc.env)
		t.Setenv("HARNESS_COWORKER", tc.confined)
		if got := DispatchOverlay(tc.flag); got != tc.want {
			t.Errorf("%s: DispatchOverlay = %q, want %q", tc.name, got, tc.want)
		}
	}
	t.Setenv(OverlayVar, "")
	t.Setenv("HARNESS_COWORKER", "")
	o, err := LoadOverlay(DispatchOverlay(""), strings.NewReader(""))
	if err != nil || o.Confinement() != "databricks:auto" {
		t.Errorf("stored overlay loaded as %q (err %v), want confinement databricks:auto", o.Confinement(), err)
	}
}
