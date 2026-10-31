package cli

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestListAgentsRegistryLoadErrorNoPanic proves listAgents survives a providers.Load() failure and no longer
// lists the folded-in "api" agent.
func TestListAgentsRegistryLoadErrorNoPanic(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(providers.EnvSessionProfile, "")
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
		listAgents(formatText) // must not panic despite a nil registry from the failed Load()
	}()

	out, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), " claude ") {
		t.Errorf("no claude row in output:\n%s", out)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == "api" {
			t.Errorf("api agent still listed: %q", line)
		}
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
				rec := agent.NewRecord("claude", agent.RunOptions{
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

// --job on a record that already finished is refused: only a fresh running, pid-less record may be run.
func TestCmdAskJobRefusesNonFreshRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.NewRecord("codex", agent.RunOptions{Prompt: "p"}, time.Now())
	rec.Status = "ok"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := captureOutput(t, func() int { return cmdAsk([]string{"--job", rec.ID}) })
	if code != 2 || !strings.Contains(stderr, "not a fresh background job") {
		t.Fatalf("exit %d stderr %q, want 2 and the not-a-fresh-job message", code, stderr)
	}
}

// attachFlag.Set rejects binary files up front (NUL, invalid UTF-8) and accepts multibyte UTF-8 text.
func TestAttachFlagSetRejectsBinary(t *testing.T) {
	dir := t.TempDir()
	var a attachFlag
	for name, data := range map[string]string{"nul.bin": "ab\x00cd", "bad.dat": "\xff\xfe\x80"} {
		f := filepath.Join(dir, name)
		writeFixtureFile(t, f, data)
		err := a.Set(f)
		if err == nil || !strings.Contains(err.Error(), "binary file") || !strings.Contains(err.Error(), name) {
			t.Errorf("Set(%s): err = %v, want a binary-file error naming the path", name, err)
		}
	}
	if len(a) != 0 {
		t.Errorf("a = %q, want nothing recorded for rejected files", a)
	}
	f := filepath.Join(dir, "utf8.txt")
	writeFixtureFile(t, f, "héllo 世界")
	if err := a.Set(f); err != nil || len(a) != 1 {
		t.Errorf("Set(multibyte text): err = %v, a = %q, want accepted", err, a)
	}
}

// agents --format json carries each agent's binary, the cost/background/live/remote levels and its event kinds.
func TestAgentsJSONCapabilitySchema(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	out := captureStdout(t, func() { listAgents(formatJSON) })
	var rows []struct {
		Name, Binary string
		Caps         map[string]string
		Events       []string
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err, out)
	}
	byName := map[string]int{}
	for i, r := range rows {
		byName[r.Name] = i
		for _, k := range []string{"cost", "background", "live", "remote", "env", "system-prompt"} {
			if r.Caps[k] == "" {
				t.Errorf("%s: caps lacks %s", r.Name, k)
			}
		}
		if r.Binary == "" || !slices.Contains(r.Events, "start") || !slices.Contains(r.Events, "result") {
			t.Errorf("%s: binary %q events %v", r.Name, r.Binary, r.Events)
		}
	}
	if c := rows[byName["claude"]]; c.Caps["live"] != "native" || c.Caps["cost"] != "native" {
		t.Errorf("claude caps = %v, want live and cost native", c.Caps)
	}
	if g := rows[byName["gemini"]]; g.Binary != "agy" || g.Caps["live"] != "native" {
		t.Errorf("gemini = %+v, want binary agy and live native", g)
	}
}

func newAskState() *askState {
	s := func() *string { return new(string) }
	b := func() *bool { return new(bool) }
	return &askState{name: s(), provider: s(), model: s(), effort: s(), workdir: s(), session: s(), rubric: s(),
		tools: b(), noTools: b(), autonomous: b(), enableMemory: b(), direct: b(),
		noPersist: b(), safeMode: b(), debugFile: s(), systemPrompt: s(), appendSystemPrompt: s(), outputSchema: s(), host: s(), remoteBin: s(), fallback: s(),
		timeout: new(time.Duration), attach: new(attachFlag), keepEnv: new(keepEnvFlag)}
}

// A retried or backgrounded dispatch must keep --no-tools; without it the tools come back on.
func TestApplyRecordRoundTripsNoTools(t *testing.T) {
	s := newAskState()
	s.applyRecord(agent.Record{Agent: "claude", Request: agent.RunOptions{NoTools: true}})
	if !*s.noTools || *s.tools {
		t.Errorf("noTools=%v tools=%v; want noTools on, tools off", *s.noTools, *s.tools)
	}
	// Negative control: a record without NoTools clears an earlier value.
	*s.noTools = true
	s.applyRecord(agent.Record{Agent: "claude", Request: agent.RunOptions{Tools: true}})
	if *s.noTools || !*s.tools {
		t.Errorf("noTools=%v tools=%v; want tools on, noTools off", *s.noTools, *s.tools)
	}
}

func TestRunLiveHolderRestoresLauncher(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var got *agent.Record
	orig := serveLive
	serveLive = func(_ context.Context, rec *agent.Record, _ func(string)) error { got = rec; return nil }
	t.Cleanup(func() { serveLive = orig })

	rec := &agent.Record{ID: "live-holder-test"}
	if rec.Request.Launcher != nil {
		t.Fatal("fixture must start without a launcher")
	}
	if code := runLiveHolder(rec); code != 0 {
		t.Fatalf("runLiveHolder = %d, want 0", code)
	}
	if got == nil || got.Request.Launcher == nil {
		t.Fatal("ServeLive got a record with no Launcher (json:\"-\" field lost on reload)")
	}
}

// lineCount counts whole lines of s equal to want.
func lineCount(s, want string) int {
	n := 0
	for l := range strings.SplitSeq(s, "\n") {
		if l == want {
			n++
		}
	}
	return n
}

// A --job run's stdout+stderr already are the log (Spawn), so the vendor child's stderr must be teed in
// exactly once and the streamed answer must not be doubled by a second stdout tee.
func TestCmdAskJobLogEachLineOnce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := agent.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "cat > /dev/null\n" +
		"echo 'child-stderr-one' >&2\n" +
		"echo 'child-stderr-two' >&2\n" +
		`printf '%s\n' '{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"answer-alpha\nanswer-beta\n"}}'` + "\n" +
		`printf '%s\n' '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"answer-alpha\nanswer-beta","num_turns":1}}'` + "\n"
	pathstest.FakeVendor(t, "agy", script)

	rec := agent.NewRecord("gemini", agent.RunOptions{Prompt: "p", WorkDir: t.TempDir()}, time.Now())
	rec.Log = rec.LogPath()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	// Mirror agent.Spawn: the job child's own stdout and stderr are the log file.
	lf, err := os.OpenFile(rec.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = lf, lf
	code := cmdAsk([]string{"--job", rec.ID, "--native", "--fallback", "none"})
	os.Stdout, os.Stderr = oldOut, oldErr
	lf.Close()
	if code != 0 {
		b, _ := os.ReadFile(rec.LogPath())
		t.Fatalf("exit %d; log:\n%s", code, b)
	}
	b, err := os.ReadFile(rec.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, line := range []string{"child-stderr-one", "child-stderr-two", "answer-alpha", "answer-beta"} {
		if n := lineCount(got, line); n != 1 {
			t.Errorf("log line %q appears %d times, want exactly once:\n%s", line, n, got)
		}
	}
	// Job mode now also streams the formatted event lines live, and never the raw NDJSON.
	if !strings.Contains(got, "▸ assistant: answer-alpha") || !strings.Contains(got, "▸ result: answer-alpha") {
		t.Errorf("log lacks the ▸ event lines:\n%s", got)
	}
	if strings.Contains(got, `{"event"`) {
		t.Errorf("log holds raw NDJSON:\n%s", got)
	}
}
