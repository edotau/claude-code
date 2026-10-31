package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A property named "properties" is data, not a schema: only real object schemas get required/additionalProperties.
func TestCodexStrictWalksSchemaKeywordsOnly(t *testing.T) {
	var v any
	src := `{"type":"object","properties":{"properties":{"type":"string"},"items":{"type":"array",` +
		`"items":{"type":"object","properties":{"n":{"type":"integer"}}}}},"$defs":{"d":{"properties":{"z":{}}}}}`
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(codexStrict(v))
	want := `{"$defs":{"d":{"additionalProperties":false,"properties":{"z":{}},"required":["z"]}},` +
		`"additionalProperties":false,"properties":{"items":{"items":{"additionalProperties":false,` +
		`"properties":{"n":{"type":"integer"}},"required":["n"],"type":"object"},"type":"array"},` +
		`"properties":{"type":"string"}},"required":["items","properties"],"type":"object"}`
	if string(got) != want {
		t.Fatalf("codexStrict =\n%s\nwant\n%s", got, want)
	}
}

// Regression: an explicit GPT model (Astra) must reach codex argv, not be swapped for the first Responses entry (Sol).
func TestCodexKeepsExplicitGPTModel(t *testing.T) {
	vaulttest.Use(t)
	menu := []providers.OpencodeModel{{ID: "gpt-6-1-sol", Responses: true}, {ID: "gpt-6-astra", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		p     *providers.Provider
		model string
	}{
		{"databricks astra", dbxRow("dev", "databricks-"), "databricks-gpt-6-astra"},
		{"non-databricks astra", &providers.Provider{Name: "other"}, "gpt-6-astra"},
	}
	for _, c := range cases {
		_, argv, err := codex(c.p, "http://127.0.0.1/x", c.model, "", &Headless{Effort: "high"})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if want := "model=" + tomlQuote(c.model); !slices.Contains(argv, want) {
			t.Errorf("%s: argv %q lacks %s", c.name, argv, want)
		}
		for _, sol := range []string{"gpt-6-1-sol", "databricks-gpt-6-1-sol"} {
			if slices.Contains(argv, "model="+tomlQuote(sol)) {
				t.Errorf("%s: argv %q routed to %s", c.name, argv, sol)
			}
		}
		if want := "model_reasoning_effort=" + tomlQuote("high"); !slices.Contains(argv, want) {
			t.Errorf("%s: argv %q lacks %s", c.name, argv, want)
		}
	}
}

// Regression (decision 24e7c144): a chat-only gpt-* model (gpt-oss) is never posted to /responses; codex swaps it.
func TestCodexChatOnlyGPTSwapped(t *testing.T) {
	vaulttest.Use(t)
	menu := []providers.OpencodeModel{{ID: "gpt-oss-120b"}, {ID: "gpt-6-1-sol", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{
		"databricks-gpt-oss-120b": "databricks-gpt-6-1-sol",
		"databricks-gpt-6-1-sol":  "databricks-gpt-6-1-sol",
	} {
		_, argv, err := codex(dbxRow("dev", "databricks-"), "http://127.0.0.1/x", model, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if w := "model=" + tomlQuote(want); !slices.Contains(argv, w) {
			t.Errorf("%s: argv %q lacks %s", model, argv, w)
		}
	}
}

// Regression: ask reported the requested model although codex's Responses swap ran another one.
func TestCodexRanModelNamesTheSwappedModel(t *testing.T) {
	vaulttest.Use(t)
	menu := []providers.OpencodeModel{{ID: "gpt-6-1-sol", Responses: true}, {ID: "gpt-6-astra", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	p := dbxRow("dev", "databricks-")
	for _, c := range []struct{ name, model, want string }{
		{"non-Responses model is swapped", "databricks-claude-opus-x", "databricks-gpt-6-1-sol"},
		{"Responses model is kept", "databricks-gpt-6-astra", "databricks-gpt-6-astra"},
	} {
		if got := codexRanModel(providers.Target{Provider: p, Model: c.model}); got != c.want {
			t.Errorf("%s: CodexRanModel(%q) = %q, want %q", c.name, c.model, got, c.want)
		}
	}
	if got := codexRanModel(providers.Target{Model: "m"}); got != "m" {
		t.Errorf("nil provider: got %q, want m", got)
	}
}

// Regression: codex on a Databricks row was handed the opus slot, which Databricks rejects on Responses.
func TestCodexResponsesModel(t *testing.T) {
	vaulttest.Use(t)
	menu := []providers.OpencodeModel{{ID: "claude-opus-5-5"}, {ID: "gpt-6-1-sol", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		p     *providers.Provider
		model string
		want  string
	}{
		{"databricks claude slot", dbxRow("dev", "databricks-"), "databricks-claude-opus-5-5", "databricks-gpt-6-1-sol"},
		{"databricks gpt kept", dbxRow("dev", "databricks-"), "databricks-gpt-6-1-sol", "databricks-gpt-6-1-sol"},
		{"non-databricks kept", &providers.Provider{Name: "other"}, "claude-opus-5-5", "claude-opus-5-5"},
	}
	for _, c := range cases {
		_, argv, err := codex(c.p, "http://127.0.0.1/x", c.model, "", nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if want := "model=" + tomlQuote(c.want); !slices.Contains(argv, want) {
			t.Errorf("%s: argv %q lacks %s", c.name, argv, want)
		}
	}
}

func TestCodexChatGPTLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no bin/codex: the auth.json fallback is what this test exercises
	t.Setenv("PATH", t.TempDir())
	tg := target(t, "chatgpt", "gpt-5.5")
	if _, _, err := schemaFor("codex").Configure(context.Background(), tg, ""); err == nil || !strings.Contains(err.Error(), "codex login") {
		t.Fatalf("no auth.json: err = %v, want a codex login hint", err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"OPENAI_API_KEY":null,"tokens":{"id_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, argv, err := schemaFor("codex").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "-c", `model_provider="openai"`, "-c", `model="gpt-5.5"`, "--skip-git-repo-check", "--sandbox", "read-only", "-"}
	if len(env) != 0 || !slices.Equal(argv, want) {
		t.Errorf("env %q argv %q, want no env and %q", env, argv, want)
	}
	if _, _, err := schemaFor("gemini").Configure(context.Background(), tg, ""); err == nil || !strings.Contains(err.Error(), "codex sign-in") {
		t.Errorf("only codex may use the chatgpt login: %v", err)
	}
}

// codexHeadless carries CODEX_EFFORT into -c model_reasoning_effort when the call names no
// explicit Effort; an explicit Headless.Effort still wins over the pin (TestConfigure's codex/Effort
// case already proves the no-pin explicit path, so this proves the pin-only and pin-vs-explicit paths).
func TestCodexHeadlessEffortPinUsedWhenEffortEmptyExplicitWinsOverIt(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	setPin(t, providers.PinCodexEffort, "")
	if err := providers.SetPins(map[string]string{providers.PinCodexEffort: ""}); err != nil {
		t.Fatal(err)
	}

	_, argv, err := schemaFor("codex").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "model_reasoning_effort=\"\"") || slices.ContainsFunc(argv, func(s string) bool { return strings.Contains(s, "model_reasoning_effort=") }) {
		t.Errorf("argv %q with no pin and no explicit effort, want no model_reasoning_effort flag", argv)
	}

	if err := providers.SetPins(map[string]string{providers.PinCodexEffort: "xhigh"}); err != nil {
		t.Fatal(err)
	}
	_, argv, err = schemaFor("codex").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="xhigh"`) {
		t.Errorf("argv = %q, want the CODEX_EFFORT pin (xhigh) carried in", argv)
	}

	// An explicit Headless.Effort still wins over the pin.
	_, argv, err = schemaFor("codex").configureHeadless(context.Background(), tg, Headless{Prompt: "hi", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="low"`) || slices.Contains(argv, `model_reasoning_effort="xhigh"`) {
		t.Errorf("argv = %q, want the explicit effort (low) to beat the xhigh pin", argv)
	}
}

func launchCodexProfile(t *testing.T, profile string, withFile bool) error {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("PATH", t.TempDir())
	if withFile {
		if err := os.WriteFile(filepath.Join(codexHome, profile+".config.toml"), []byte("model = \"x\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opts, rest, err := ParseArgs([]string{"--profile", profile}, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LaunchPlan(context.Background(), "codex", opts, rest)
	return err
}

func TestCodexProfileMatchesConfigFileHintsDashP(t *testing.T) {
	err := launchCodexProfile(t, "work", true)
	if err == nil || !strings.Contains(err.Error(), "use -p work") {
		t.Fatalf("err = %v, want message containing %q", err, "use -p work")
	}
}

func TestCodexProfileUnknownKeepsOldError(t *testing.T) {
	err := launchCodexProfile(t, "nosuch", false)
	if err == nil || !strings.Contains(err.Error(), `unknown provider "nosuch"`) || strings.Contains(err.Error(), "use -p") {
		t.Fatalf("err = %v, want the unknown-provider error without a -p hint", err)
	}
}

// TestCodexFileOverrides flattens top-level keys and nested tables into -c pairs and refuses multi-line values.
func TestCodexFileOverrides(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.toml")
	os.WriteFile(good, []byte("# c\nmodel = \"m\"\n\n[model_providers.d]\nbase_url = \"https://h\"\n[model_providers.d.auth]\nargs = [\"resolve\", \"--provider\", \"qa\"]\n"), 0o600)
	got, err := codexFileOverrides(good)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-c", `model="m"`, "-c", `model_providers.d.base_url="https://h"`,
		"-c", `model_providers.d.auth.args=["resolve", "--provider", "qa"]`}
	if !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	bad := filepath.Join(dir, "bad.toml")
	os.WriteFile(bad, []byte("args = [\n  \"x\",\n]\n"), 0o600)
	if _, err := codexFileOverrides(bad); err == nil || !strings.Contains(err.Error(), ":1:") {
		t.Errorf("multi-line value: err = %v, want a line-1 error", err)
	}
}

// TestParseArgsFileFlag: -f/--file is taken on vendor launches only.
func TestParseArgsFileFlag(t *testing.T) {
	for _, args := range [][]string{{"-f", "x.toml", "exec"}, {"--file=x.toml", "exec"}} {
		o, rest, err := ParseArgs(args, false)
		if err != nil || o.File != "x.toml" || !slices.Equal(rest, []string{"exec"}) {
			t.Errorf("ParseArgs(%q) = %+v, %q, %v", args, o, rest, err)
		}
	}
	if o, rest, _ := ParseArgs([]string{"-f", "x"}, true); o.File != "" || len(rest) != 2 {
		t.Errorf("Claude launch must pass -f through: %+v %q", o, rest)
	}
}

// exitErrorWithCode runs a trivial subprocess that exits with code after writing stderr, returning a genuine
// *exec.ExitError (Stderr populated, as Output() does) — the OS keychain tool's exit semantics without the keyring.
func exitErrorWithCode(t *testing.T, code int, stderr string) *exec.ExitError {
	t.Helper()
	script := "exit " + strconv.Itoa(code)
	if stderr != "" {
		script = "echo " + stderr + " >&2; " + script
	}
	_, err := exec.Command("/bin/sh", "-c", script).Output()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("exitErrorWithCode(%d): want *exec.ExitError, got %T (%v)", code, err, err)
	}
	return ee
}

// keyringMissErr is the current OS's "nothing stored" exit: security exits 44, secret-tool exits 1 with no stderr.
func keyringMissErr(t *testing.T) *exec.ExitError {
	t.Helper()
	if runtime.GOOS == "darwin" {
		return exitErrorWithCode(t, 44, "")
	}
	return exitErrorWithCode(t, 1, "")
}

// codexAccountJSON is the fixture body the keyring (or file) returns: a well-formed codex auth.json payload.
func codexAccountJSON(accessToken string) string {
	return `{"auth_mode":"chatgpt","tokens":{"access_token":"` + accessToken + `","account_id":"acc_1"}}`
}

// chatgptAccessToken is an unsigned JWT carrying a chatgpt_account_id claim, matching the plans_test fixtures.
var chatgptAccessToken = unsignedJWT(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acc_1"}}`)

// noVendorBin points BinDir() (paths.VendorBin reads $CLAUDE_CONFIG_DIR/bin, not $PATH) at an empty dir so
// paths.VendorBin("codex") never finds a real CLI to shell out to.
func noVendorBin(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

func TestCodexAuthJSON_FilePresentKeyringNeverCalled(t *testing.T) {
	noVendorBin(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	body := codexAccountJSON(chatgptAccessToken)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	called := false
	orig := codexKeychainCmd
	codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) {
		called = true
		return nil, errors.New("keyring must not be consulted when auth.json is present")
	}
	defer func() { codexKeychainCmd = orig }()

	data, note := codexAuthJSON()
	if string(data) != body {
		t.Errorf("codexAuthJSON data = %q, want %q", data, body)
	}
	if note != "" {
		t.Errorf("codexAuthJSON keyringNote = %q, want empty", note)
	}
	if called {
		t.Error("codexAuthJSON called the keyring seam despite auth.json being present")
	}

	token, accountID, keyringNote := CodexAccessToken()
	if token != chatgptAccessToken || accountID != "acc_1" || keyringNote != "" {
		t.Errorf("CodexAccessToken = (%q, %q, %q), want (%q, %q, \"\")", token, accountID, keyringNote, chatgptAccessToken, "acc_1")
	}
}

func TestCodexAuthJSON_FileAbsentKeyringHit(t *testing.T) {
	noVendorBin(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home) // no auth.json written: forces the keyring fallback

	body := codexAccountJSON(chatgptAccessToken)
	orig := codexKeychainCmd
	codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) { return []byte(body), nil }
	defer func() { codexKeychainCmd = orig }()

	token, accountID, keyringNote := CodexAccessToken()
	if token != chatgptAccessToken {
		t.Errorf("CodexAccessToken token = %q, want %q", token, chatgptAccessToken)
	}
	if accountID != "acc_1" {
		t.Errorf("CodexAccessToken accountID = %q, want %q", accountID, "acc_1")
	}
	if keyringNote != "" {
		t.Errorf("CodexAccessToken keyringNote = %q, want empty on a keyring hit", keyringNote)
	}
	if !CodexChatGPTLoginPresent() {
		t.Error("CodexChatGPTLoginPresent() = false, want true when the keyring holds a valid ChatGPT auth blob")
	}
}

func TestCodexAuthJSON_FileAbsentKeyringMissTreatedAsAbsent(t *testing.T) {
	noVendorBin(t)

	cases := []struct {
		name string
		err  error
		out  []byte
	}{
		{"keyring tool: item not found", keyringMissErr(t), nil},
		{"empty output, no error", nil, []byte("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			orig := codexKeychainCmd
			codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) { return c.out, c.err }
			defer func() { codexKeychainCmd = orig }()

			token, accountID, keyringNote := CodexAccessToken()
			if token != "" || accountID != "" {
				t.Errorf("CodexAccessToken = (%q, %q), want (\"\", \"\") on keyring miss", token, accountID)
			}
			if keyringNote != "" {
				t.Errorf("CodexAccessToken keyringNote = %q, want empty on a plain keyring miss", keyringNote)
			}
			if CodexChatGPTLoginPresent() {
				t.Error("CodexChatGPTLoginPresent() = true, want false on keyring miss")
			}
		})
	}
}

func TestCodexAuthJSON_KeyringErrorOtherThanAbsentNotedAsKeyring(t *testing.T) {
	noVendorBin(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	orig := codexKeychainCmd
	codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, exitErrorWithCode(t, 1, "keyring is locked") // non-empty stderr: a real failure on every OS
	}
	defer func() { codexKeychainCmd = orig }()

	_, _, keyringNote := CodexAccessToken()
	if keyringNote == "" {
		t.Error("CodexAccessToken keyringNote = \"\", want a note for a non-absent keyring error")
	}

	err := codexChatGPTLogin()
	if err == nil {
		t.Fatal("codexChatGPTLogin() = nil, want an error surfacing the keyring failure")
	}
	if !strings.Contains(err.Error(), "keyring:") {
		t.Errorf("codexChatGPTLogin() error = %q, want it to mention \"keyring:\"", err)
	}
}

func TestCodexAuthJSON_CorruptKeyringJSONTreatedAsAbsent(t *testing.T) {
	noVendorBin(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	orig := codexKeychainCmd
	codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) { return []byte("{not json"), nil }
	defer func() { codexKeychainCmd = orig }()

	token, accountID, _ := CodexAccessToken()
	if token != "" || accountID != "" {
		t.Errorf("CodexAccessToken = (%q, %q), want (\"\", \"\") on corrupt keyring JSON", token, accountID)
	}
	if CodexChatGPTLoginPresent() {
		t.Error("CodexChatGPTLoginPresent() = true, want false on corrupt keyring JSON")
	}
}
