package cli

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestOnPassthroughProvider pins the api-row helper's two outcomes: the default target's provider
// forwards the caller's own auth (passthrough) vs. calls the vendor directly (api key).
func TestOnPassthroughProvider(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HARNESS_PROVIDER", "") // a session's own pin must not leak into this test
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"fakepass","providers":{
	  "fakepass":{"kind":"anthropic","base_url":"https://example.invalid","auth":{"type":"passthrough"},"models":{"opus":"x"}}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatalf("providers.Load() = %v", err)
	}
	if !onPassthroughProvider(reg) {
		t.Error("onPassthroughProvider(reg) = false, want true for a passthrough default provider")
	}
}

func TestOnPassthroughProviderFalseForAPIKeyProvider(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HARNESS_PROVIDER", "") // a session's own pin must not leak into this test
	t.Setenv("TEST_FAKE_KEY", "k")
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"fakekey","providers":{
	  "fakekey":{"kind":"anthropic","base_url":"https://example.invalid","auth":{"type":"x-api-key","env":"TEST_FAKE_KEY"},"models":{"opus":"x"}}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatalf("providers.Load() = %v", err)
	}
	if onPassthroughProvider(reg) {
		t.Error("onPassthroughProvider(reg) = true, want false for an api-key default provider")
	}
}

// TestListRunnersRegistryLoadErrorNoPanic proves listRunners handles a providers.Load() failure
// without dereferencing the nil registry it gets back: the "api" row must show "✗" with the load
// error as its reason, and the call must not panic for any other runner row either.
func TestListRunnersRegistryLoadErrorNoPanic(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HARNESS_PROVIDER", "")
	vaulttest.Put(t, vault.Overlay, "providers", "{not valid json")
	if _, err := providers.Load(); err == nil {
		t.Fatal("providers.Load() must fail on malformed overlay JSON for this test to prove anything")
	}

	tmp := filepath.Join(t.TempDir(), "stdout.txt")
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	func() {
		defer func() { os.Stdout = old; f.Close() }()
		listRunners() // must not panic despite a nil registry from the failed Load()
	}()

	out, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "api" {
			if fields[0] != "✗" {
				t.Errorf("api row mark = %q, want ✗: %q", fields[0], line)
			}
			if !strings.Contains(line, "not valid json") && !strings.Contains(strings.ToLower(line), "invalid character") {
				t.Errorf("api row must name the load error as its reason: %q", line)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no api row found in output:\n%s", out)
	}
}

// writeFixtureFile creates path (with parents) holding content.
func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// (i) CLI wiring: `agents tree --format json` resolves claudeDir from CLAUDE_CONFIG_DIR, dispatches
// through cmdAgents, and emits the documented {sessions:[...]} schema on stdout.
func TestCmdAgentsTreeFormatJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeFixtureFile(t, filepath.Join(dir, "projects", "-Users-fake-repo", "sess0001.jsonl"),
		`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"hello"},"cwd":"/Users/fake/repo","gitBranch":"main"}`)

	code, stdout, stderr := captureOutput(t, func() int { return cmdAgents([]string{"tree", "--days", "365", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("agents tree --format json exit %d, stderr=%s", code, stderr)
	}
	var decoded struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("unmarshal stdout: %v\nstdout: %s", err, stdout)
	}
	if len(decoded.Sessions) != 1 || decoded.Sessions[0].ID != "sess0001" {
		t.Errorf("sessions = %+v, want one session sess0001", decoded.Sessions)
	}
}

// agents tree rejects an unknown --format value before touching the filesystem.
func TestCmdAgentsTreeRejectsBadFormat(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return cmdAgents([]string{"tree", "--format", "xml"}) })
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if stderr == "" {
		t.Error("expected an error on stderr for an unknown --format value")
	}
}

// attachFlag.Set resolves a real file to an absolute path and errors on anything else: missing,
// directory, or a path flag.Abs itself cannot resolve.
func TestAttachFlagSet(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	writeFixtureFile(t, f, "hi")

	var a attachFlag
	if err := a.Set(f); err != nil {
		t.Fatalf("Set(%q): %v", f, err)
	}
	if len(a) != 1 || a[0] != f {
		t.Errorf("a = %q, want [%q]", a, f)
	}

	if err := a.Set(filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("Set on a missing path: want an error, got nil")
	}
	if err := a.Set(dir); err == nil {
		t.Error("Set on a directory: want an error, got nil")
	}
}

// keepEnvFlag.Set comma-splits each occurrence and accumulates across repeats.
func TestKeepEnvFlagSet(t *testing.T) {
	var k keepEnvFlag
	if err := k.Set("DATABRICKS_HOST,DATABRICKS_TOKEN"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := k.Set("HOME"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !slices.Equal([]string(k), []string{"DATABRICKS_HOST", "DATABRICKS_TOKEN", "HOME"}) {
		t.Errorf("k = %q, want accumulated comma-split names", k)
	}
}

// timeoutExplicit distinguishes a --timeout the caller typed from the flag's own zero-cost default,
// so the --tools-aware 45m default never clobbers an explicit value.
func TestTimeoutExplicit(t *testing.T) {
	newFS := func() (*flag.FlagSet, *time.Duration) {
		fs := flag.NewFlagSet("ask", flag.ContinueOnError)
		return fs, fs.Duration("timeout", 10*time.Minute, "")
	}
	fs, _ := newFS()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if timeoutExplicit(fs) {
		t.Error("timeoutExplicit() = true with no flags parsed, want false")
	}

	fs, timeout := newFS()
	if err := fs.Parse([]string{"-timeout", "5m"}); err != nil {
		t.Fatal(err)
	}
	if !timeoutExplicit(fs) {
		t.Error("timeoutExplicit() = false with -timeout on the command line, want true")
	}
	if *timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", *timeout)
	}
}

func TestCmdAskRetryFlags(t *testing.T) {
	for _, tt := range []struct {
		name        string
		retry       bool
		flags       []string
		attachments []string
		wantTimeout time.Duration
		wantAttach  []string
		wantKeepEnv []string
	}{
		{name: "retry keeps recorded timeout and attachments", retry: true,
			wantTimeout: 5 * time.Minute, wantAttach: []string{"a.txt"}},
		{name: "retry typed timeout wins", retry: true, flags: []string{"--timeout", "2m"},
			wantTimeout: 2 * time.Minute, wantAttach: []string{"a.txt"}},
		{name: "fresh tools default", flags: []string{"--tools"},
			wantTimeout: 45 * time.Minute},
		{name: "fresh tools typed timeout wins", flags: []string{"--tools", "--timeout", "5m"},
			wantTimeout: 5 * time.Minute},
		{name: "fresh without tools default", wantTimeout: 10 * time.Minute},
		{name: "retry attachment replaces recorded", retry: true, attachments: []string{"b.txt"},
			wantTimeout: 5 * time.Minute, wantAttach: []string{"b.txt"}},
		{name: "retry repeated attachments accumulate", retry: true, attachments: []string{"b.txt", "c.txt"},
			wantTimeout: 5 * time.Minute, wantAttach: []string{"b.txt", "c.txt"}},
		{name: "fresh --keep-env is recorded", flags: []string{"--keep-env", "DATABRICKS_HOST,HOME"},
			wantTimeout: 10 * time.Minute, wantKeepEnv: []string{"DATABRICKS_HOST", "HOME"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			t.Setenv("HOME", dir)
			t.Setenv("PATH", dir)
			for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
				writeFixtureFile(t, filepath.Join(dir, name), name)
			}
			args := []string{"--agent", "claude", "--fallback", "none", "--format", "json"}
			wantRecords := 1
			if tt.retry {
				rec := agent.NewRecord("claude", agent.Request{
					Prompt: "say ok", Tools: true, Timeout: 5 * time.Minute,
					Attach: []string{filepath.Join(dir, "a.txt")},
				}, time.Now().Add(-time.Hour))
				if err := rec.Save(); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--retry", rec.ID)
				wantRecords++
			}
			args = append(args, tt.flags...)
			for _, name := range tt.attachments {
				args = append(args, "--attach", filepath.Join(dir, name))
			}
			if !tt.retry {
				args = append(args, "say ok")
			}
			code, _, stderr := captureOutput(t, func() int { return cmdAsk(args) })
			if code != 1 {
				t.Fatalf("exit = %d, want 1 for unavailable runner; stderr=%s", code, stderr)
			}
			records := agent.RecentRecords(2)
			if len(records) != wantRecords {
				t.Fatalf("records = %d, want %d; stderr=%s", len(records), wantRecords, stderr)
			}
			req := records[0].Request
			if req.Timeout != tt.wantTimeout {
				t.Errorf("timeout = %v, want %v", req.Timeout, tt.wantTimeout)
			}
			var wantAttach []string
			for _, name := range tt.wantAttach {
				wantAttach = append(wantAttach, filepath.Join(dir, name))
			}
			if !slices.Equal(req.Attach, wantAttach) {
				t.Errorf("attachments = %q, want %q", req.Attach, wantAttach)
			}
			if !slices.Equal(req.KeepEnv, tt.wantKeepEnv) {
				t.Errorf("keepEnv = %q, want %q", req.KeepEnv, tt.wantKeepEnv)
			}
		})
	}
}

// --background and --job are mutually exclusive: cmdAsk must fail before doing any work.
func TestCmdAskBackgroundAndJobMutuallyExclusive(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return cmdAsk([]string{"--background", "--job", "some-id", "say ok"})
	})
	if code == 0 {
		t.Fatalf("exit = %d, want non-zero for --background with --job; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("stderr = %q, want a mutually-exclusive message", stderr)
	}
}

// agents tree's text output (the default) renders without error when no sessions are in scope.
func TestCmdAgentsTreeTextNoSessions(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, _ := captureOutput(t, func() int { return cmdAgents([]string{"tree"}) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout == "" {
		t.Error("expected a \"no sessions in scope\" message on stdout")
	}
}
