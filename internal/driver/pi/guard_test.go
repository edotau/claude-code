package pi

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/rtkwrap"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/providers"
)

func TestGuardWritesPrivateExtension(t *testing.T) {
	dir := t.TempDir()
	g, err := WriteGuard(dir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(g.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--pi-guard", "HARNESS_HOOK_", "VERIFIED_PI", "registerTool", "tool_call", "getAllTools", "sourceInfo", "session_start", "realpathSync(p)", "realpathSync(fileURLToPath"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("extension lacks %q", want)
		}
	}
	if strings.Contains(string(body), "__BIN__") || strings.Contains(string(body), "__PI_VERSION__") || strings.Contains(string(body), "__BUDGET_MS__") {
		t.Error("placeholder left unrendered")
	}
	if info, _ := os.Stat(g.Path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestGuardArgvAndEnv(t *testing.T) {
	g, err := WriteGuard(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a := g.Argv(); len(a) != 2 || a[0] != "-e" || a[1] != g.Path {
		t.Errorf("argv = %v", a)
	}
	if e := strings.Join(g.Env(), " "); !strings.Contains(e, nonceFileEnv+"="+g.NonceFile) || !strings.Contains(e, nonceValueEnv+"=") {
		t.Errorf("env = %v", g.Env())
	}
}

func TestGuardReadyNeedsTheNonce(t *testing.T) {
	g, err := WriteGuard(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if g.Ready() == nil {
		t.Fatal("ready before the extension wrote the nonce")
	}
	if err := os.WriteFile(g.NonceFile, []byte("stale"), 0o600); err != nil || g.Ready() == nil {
		t.Fatal("a wrong nonce must not be ready")
	}
	if err := os.WriteFile(g.NonceFile, []byte(g.Nonce), 0o600); err != nil || g.Ready() != nil {
		t.Fatalf("right nonce not ready: %v", g.Ready())
	}
}

func TestGuardPinsVerifiedPi(t *testing.T) {
	body, err := renderGuard("/bin/x", 7000)
	if err != nil || !strings.Contains(string(body), "VERIFIED_PI: string = "+strconv.Quote(tools.PiPin)) {
		t.Fatalf("guard does not carry the install pin %s: %v", tools.PiPin, err)
	}
}

func TestGuardDefinesMsgAction(t *testing.T) {
	g, err := WriteGuard(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(g.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\n  msg: { label:", `const MSG_READ = ["agents", "msg"]`, `pi.on("before_agent_start"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("guard lacks %s", want)
		}
	}
	if !slices.Contains(strings.Split(guardTools, ","), "msg") {
		t.Error("guardTools lacks msg")
	}
}

func TestExperimentIsOneTrial(t *testing.T) {
	wrap := map[bool]string{true: "none", false: "rtk-wrap"}[runtime.GOOS == "windows"]
	for _, c := range []struct {
		codemode string
		bash     bool
		want     string
	}{
		{"", false, "none"}, {"off", true, wrap}, {"", true, wrap}, {"on", true, "codemode"}, {"only", false, "codemode"},
	} {
		if got := experiment(c.codemode, c.bash); got != c.want {
			t.Errorf("experiment(%q, %v) = %q, want %q", c.codemode, c.bash, got, c.want)
		}
	}
}

func TestResolveCodemode(t *testing.T) {
	vaulttest.Use(t)
	for name, c := range map[string]struct {
		typed, setting, want string
		tools                bool
	}{
		"setting":     {"", "only", "only", true},
		"case":        {"", " On ", "on", true},
		"typed wins":  {"on", "only", "on", true},
		"typed off":   {"off", "on", "off", true},
		"setting off": {"", "off", "", true},
		"no tools":    {"", "only", "", false},
		"bad value":   {"", "yes", "", true},
		"unset":       {"", "", "", true},
	} {
		if err := providers.SetCustomEnv(rtkwrap.CodemodeEnv, c.setting); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveCodemode(c.typed, c.tools)
		if got != c.want || err != nil {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, c.want)
		}
	}
}

func TestResolveCodemodeIgnoresAmbientEnv(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(rtkwrap.CodemodeEnv, "on")
	if got, err := ResolveCodemode("", true); got != "" || err != nil {
		t.Fatalf("ambient CODEMODE=on resolved to %q, %v; want off", got, err)
	}
}

func TestCodemodeOffOverridesVault(t *testing.T) {
	vaulttest.Use(t)
	if err := providers.SetCustomEnv(rtkwrap.CodemodeEnv, "on"); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveCodemode("", true); got != "on" || err != nil {
		t.Fatalf("vault control = %q, %v; want on", got, err)
	}
	for _, tools := range []bool{true, false} {
		got, err := ResolveCodemode("off", tools)
		if got != "off" || err != nil {
			t.Fatalf("off with tools=%v = %q, %v; want off kept", tools, got, err)
		}
		if err := checkCodemode(agent.RunOptions{Tools: tools, Codemode: got}, nil); err != nil {
			t.Fatal(err)
		}
		if argv := codemodeArgv(got); len(argv) != 0 || experiment(got, false) != "none" {
			t.Fatalf("off enables codemode: argv=%v, experiment=%q", argv, experiment(got, false))
		}
	}
}

func TestCheckCodemode(t *testing.T) {
	for name, c := range map[string]struct {
		req agent.RunOptions
		ok  bool
	}{
		"off":      {agent.RunOptions{}, true},
		"on":       {agent.RunOptions{Tools: true, Codemode: "on"}, true},
		"bad mode": {agent.RunOptions{Tools: true, Codemode: "yes"}, false},
		"no tools": {agent.RunOptions{Codemode: "only"}, false},
	} {
		if err := checkCodemode(c.req, nil); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
		}
	}
}

func TestCodemodeLaunchEnvExportsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shims are unix-only")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(rtkwrap.Binary()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rtkwrap.Binary(), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := codemodeLaunchEnv([]string{rtkwrap.CodemodeEnv + "=stale"}, "only")
	if got := proc.LastEnv(env, rtkwrap.CodemodeEnv); got != "only" {
		t.Errorf("%s = %q, want only", rtkwrap.CodemodeEnv, got)
	}
	if !strings.HasPrefix(proc.LastEnv(env, "PATH"), rtkwrap.Dir()) || proc.LastEnv(env, "BASH_ENV") != "" {
		t.Errorf("code mode: PATH = %q, BASH_ENV = %q", proc.LastEnv(env, "PATH"), proc.LastEnv(env, "BASH_ENV"))
	}
}

func TestWriteCodemodeSettingsKeepsTheRunSettings(t *testing.T) {
	dir := t.TempDir()
	if err := WriteConfig(dir, []byte(`{}`), []byte(`{"defaultModel":"m"}`)); err != nil {
		t.Fatal(err)
	}
	if err := writeCodemodeSettings([]string{agentDirEnv + "=" + dir}, "only"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dir + "/settings.json")
	if err != nil || !strings.Contains(string(raw), `"mode": "only"`) || !strings.Contains(string(raw), `"defaultModel": "m"`) {
		t.Errorf("settings.json = %s, %v", raw, err)
	}
}

func TestHeadlessPromptNeverParsesAsAFlag(t *testing.T) {
	for prompt, want := range map[string]string{"-a": "-a", "--approve": "--approve", "@secret": " @secret", "a@b": "a@b"} {
		p := &agent.CLIPrep{Prompt: prompt}
		piEngine{}.After(p, agent.RunOptions{})
		if n := len(p.Argv); n < 2 || p.Argv[n-2] != "--" || p.Argv[n-1] != want {
			t.Errorf("prompt %q: argv tail %v, want -- %q", prompt, p.Argv, want)
		}
	}
}
