package databricks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const exHost = "https://example.cloud.databricks.com"

func noTokens(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, "ANTHROPIC_AUTH_TOKEN") || strings.Contains(s, "eyJ") {
		t.Errorf("token-like string in output:\n%s", s)
	}
}

func TestConfigStdoutClaudeAndCodex(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, out, _ := runCaptured(t, func() int {
		return config([]string{"claude", "--host", exHost, "--wire", "serving", "--auth", "cli", "--set", "FOO=1", "--unset", "ENABLE_TOOL_SEARCH"})
	})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	noTokens(t, out)
	for _, w := range []string{exHost + "/serving-endpoints/anthropic", `"FOO": "1"`, "databricks auth token --host " + exHost} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in\n%s", w, out)
		}
	}
	if strings.Contains(out, "ENABLE_TOOL_SEARCH") {
		t.Error("unset ignored")
	}
	code, out, _ = runCaptured(t, func() int {
		return config([]string{"codex", "--host", exHost, "--auth", "cli"})
	})
	if code != 0 || !strings.Contains(out, "[model_providers.databricks.auth]") || !strings.Contains(out, exHost+"/serving-endpoints") {
		t.Errorf("codex %d:\n%s", code, out)
	}
	noTokens(t, out)
}

func TestConfigGatewayNoteOnStderr(t *testing.T) {
	_, out, errOut := runCaptured(t, func() int {
		return config([]string{"claude", "--host", exHost, "--wire", "gateway", "--auth", "cli"})
	})
	if !strings.Contains(errOut, "budget-denied") || strings.Contains(out, "budget-denied") {
		t.Errorf("note placement: out=%q err=%q", out, errOut)
	}
	if !strings.Contains(out, exHost+"/ai-gateway/anthropic") {
		t.Errorf("gateway url missing:\n%s", out)
	}
}

func TestConfigRefusals(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := map[string][]string{
		"bad target":  {"vim", "--host", exHost},
		"cred --set":  {"claude", "--host", exHost, "--set", "ANTHROPIC_AUTH_TOKEN=x"},
		"bad wire":    {"claude", "--host", exHost, "--wire", "nope"},
		"bad auth":    {"claude", "--host", exHost, "--auth", "nope"},
		"non-claude":  {"claude", "--host", exHost, "--auth", "cli", "--model", "gpt-5.5"},
		"claude->cdx": {"codex", "--host", exHost, "--auth", "cli", "--model", "claude-opus-5-5"},
		"env+prov":    {"claude", "--env", "dev", "--provider", "x"},
		"bad env":     {"claude", "--env", "staging", "--host", exHost},
		"http host":   {"claude", "--host", "http://example.com", "--auth", "cli"},
	}
	for name, a := range cases {
		code, out, _ := runCaptured(t, func() int { return config(a) })
		if code != 2 {
			t.Errorf("%s: code %d", name, code)
		}
		if out != "" {
			t.Errorf("%s: wrote stdout %q", name, out)
		}
	}
}

func TestConfigOutGuard(t *testing.T) {
	out := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(out, []byte("KEEP"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"claude", "--host", exHost, "--auth", "cli", "--out", out}
	if code := config(args); code != 2 {
		t.Fatalf("overwrite w/o --force: %d", code)
	}
	if b, _ := os.ReadFile(out); string(b) != "KEEP" {
		t.Errorf("file clobbered: %q", b)
	}
	if code := config(append(args, "--force")); code != 0 {
		t.Fatalf("force: %d", code)
	}
	b, _ := os.ReadFile(out)
	noTokens(t, string(b))
	if !strings.Contains(string(b), exHost) {
		t.Errorf("not rewritten:\n%s", b)
	}
	fresh := filepath.Join(t.TempDir(), "n.json")
	if code := config([]string{"claude", "--host", exHost, "--auth", "cli", "--out", fresh}); code != 0 {
		t.Fatal(code)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
}

func fakeClaudeOnPath(t *testing.T) {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "claude"), []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", d)
}

func TestRescuePrintNeverExecs(t *testing.T) {
	fakeClaudeOnPath(t)
	f := &fakeChild{}
	useFake(t, f)
	code, out, _ := runCaptured(t, func() int { return rescue([]string{"--host", exHost, "--print", "--", "-p", "hi"}) })
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if len(f.calls) != 0 {
		t.Fatalf("--print spawned: %v", f.calls)
	}
	noTokens(t, out)
	i := strings.Index(out, "claude --settings ")
	if i < 0 {
		t.Fatalf("no command line:\n%s", out)
	}
	file := strings.Fields(out[i:])[2]
	if !strings.HasPrefix(file, os.TempDir()) {
		t.Errorf("scratch outside temp: %s", file)
	}
	t.Cleanup(func() { os.Remove(file) })
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("scratch %v %v", fi, err)
	}
	if !strings.Contains(out, "-p hi") || !strings.Contains(out, "databricks auth token --host "+exHost) {
		t.Errorf("args/auth default missing:\n%s", out)
	}
}

func TestRescueRunsThroughSeamWithScrubbedEnv(t *testing.T) {
	fakeClaudeOnPath(t)
	t.Setenv("ANTHROPIC_BASE_URL", "http://leak")
	t.Setenv("HARNESS_OPUS_MODEL", "leak")
	f := &fakeChild{code: 7}
	useFake(t, f)
	code, _, _ := runCaptured(t, func() int { return rescue([]string{"--host", exHost, "--", "-p", "hi"}) })
	if code != 7 || len(f.calls) != 1 {
		t.Fatalf("code %d calls %d", code, len(f.calls))
	}
	c := f.calls[0]
	t.Cleanup(func() { os.Remove(argValue(c.argv, "--settings")) })
	if !strings.HasSuffix(c.bin, "claude") || c.argv[0] != "--settings" || c.argv[len(c.argv)-1] != "hi" {
		t.Errorf("call %+v", c)
	}
	for _, kv := range c.env {
		if strings.HasPrefix(kv, "ANTHROPIC_") || strings.HasPrefix(kv, "HARNESS_") {
			t.Errorf("env leaked %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
}

func TestRescueRefusals(t *testing.T) {
	fakeClaudeOnPath(t)
	f := &fakeChild{}
	useFake(t, f)
	for name, a := range map[string][]string{
		"cred set":   {"--host", exHost, "--set", "MY_SECRET=1"},
		"non-claude": {"--host", exHost, "--model", "gpt-5.5"},
		"bad wire":   {"--host", exHost, "--wire", "x"},
	} {
		if code, _, _ := runCaptured(t, func() int { return rescue(a) }); code != 2 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(f.calls) != 0 {
		t.Error("refused rescue spawned")
	}
	t.Setenv("PATH", t.TempDir())
	if code, _, _ := runCaptured(t, func() int { return rescue([]string{"--host", exHost}) }); code != 2 || len(f.calls) != 0 {
		t.Errorf("missing claude: %d", code)
	}
}
