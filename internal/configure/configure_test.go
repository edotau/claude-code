package configure

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A piped --check on a box with no keys reports every absent credential, writes nothing, and exits 1.
func TestCheckPipedReportsAbsentKeysAndExitsOne(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", t.TempDir()) // no gh/codex/copilot on PATH: nothing can look "present" by accident
	var out bytes.Buffer
	rc := Run(Options{Check: true, Home: home, In: strings.NewReader(""), Out: &out}, "")
	if rc != 1 {
		t.Errorf("rc = %d, want 1; output:\n%s", rc, out.String())
	}
	for _, want := range []string{"== keys ==", "ABSENT", "claude-code secrets set OPENAI_API_KEY"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Errorf("--check wrote into HOME: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(cfg, "env.d", "secrets.env")); err == nil {
		t.Error("--check wrote secrets.env")
	}
}

// A vendor whose only resolvable provider is another vendor's passthrough login is ABSENT, not "present".
func TestVendorKeyTargetRejectsForeignPassthrough(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("HARNESS_PROVIDER", "subscription")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	_ = Run(Options{Check: true, Vendor: "codex", Home: home, In: strings.NewReader(""), Out: &out}, "keys")
	if !strings.Contains(out.String(), "codex") || strings.Contains(out.String(), "codex      present (subscription") {
		t.Errorf("codex must not read as present on a subscription session:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "codex login") {
		t.Errorf("codex row should name `codex login` as the fix:\n%s", out.String())
	}
}

// A vendor running on its own stored config (opencode.jsonc sets `model`) is configured, not ABSENT: the runner
// launches it untouched, so `--check` must not demand a key for it.
func TestVendorOnOwnConfigReadsPresent(t *testing.T) {
	cfg, home, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "subscription")
	t.Setenv("PATH", t.TempDir())
	if err := os.MkdirAll(filepath.Join(xdg, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "opencode", "opencode.jsonc"), []byte(`{"model": "google/gemini-3.8-flash"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rc := Run(Options{Check: true, Vendor: "opencode", Home: home, In: strings.NewReader(""), Out: &out}, "keys")
	if rc != 0 || !strings.Contains(out.String(), "opencode   present (opencode's own config (google/gemini-3.8-flash))") {
		t.Errorf("rc = %d; own-config vendor must read present:\n%s", rc, out.String())
	}
}

// An absent credential offers BOTH the browser login and a pasted key; piped, each is one [Y/n], so declining
// the login and accepting the key writes it to env.d/secrets.env without ever opening a browser.
func TestResolveKeyOffersLoginAndKeyThenStoresPastedKey(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	kt := keyTarget{label: "gemini", envVar: "GEMINI_API_KEY", loginArgv: []string{"gemini"}, fixCmd: "x"}
	opts := authOptions(kt)
	if len(opts) != 2 || opts[0].argv == nil || opts[1].env != "GEMINI_API_KEY" || !strings.Contains(opts[1].label, "aistudio") {
		t.Fatalf("authOptions = %+v, want browser login then key with its console URL", opts)
	}
	in := strings.NewReader("n\ny\nn\n  sk-test-value \n")
	var out bytes.Buffer
	d := &dialog{o: Options{Interactive: true, In: in, Out: &out}, br: bufio.NewReader(in), out: &out}
	if !resolveKey(d, kt) {
		t.Fatalf("resolveKey = false\n%s", out.String())
	}
	got, err := os.ReadFile(filepath.Join(cfg, "env.d", "secrets.env"))
	if err != nil || !strings.Contains(string(got), "GEMINI_API_KEY=sk-test-value") {
		t.Errorf("secrets.env = %q (%v)", got, err)
	}
	if strings.Contains(out.String(), "logged in") {
		t.Error("login must not have run after being declined")
	}
	for _, want := range []string{"Sign in via browser (gemini)? [Y/n]", "Paste an API key (GEMINI_API_KEY)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dialog lacks %q:\n%s", want, out.String())
		}
	}
}

// The Max-plan row offers three ways in: browser login, minting a headless token, pasting one; the minted token
// is recognised in the CLI's output by its sk-ant-oat prefix.
func TestMaxPlanOptionsAndSetupTokenCapture(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no claude binary: status reads as not logged in
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	kt := maxPlanTarget(keyTarget{label: "claude"})
	if kt.present || kt.headless {
		t.Fatalf("no login and no token must read absent: %+v", kt)
	}
	opts := authOptions(kt)
	if len(opts) != 3 || opts[0].argv == nil || opts[1].mint == nil || opts[1].env != "CLAUDE_CODE_OAUTH_TOKEN" || opts[2].env != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("authOptions = %+v, want login, mint, paste", opts)
	}
	out := "Opening browser...\nPaste the code here:\n\nYour token: sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789\n"
	if got := setupTokenRE.FindString(out); got != "sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789" {
		t.Errorf("token capture = %q", got)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-stored")
	if kt := maxPlanTarget(keyTarget{label: "claude"}); !kt.present || !kt.headless || !strings.Contains(kt.source, "headless token") {
		t.Errorf("a stored token must read present/headless: %+v", kt)
	}
}
