package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/batch"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
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
				t.Fatalf("exit = %d, want 1 for unavailable agent; stderr=%s", code, stderr)
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
		tools: b(), readOnly: b(), noTools: b(), autonomous: b(), enableMemory: b(), direct: b(),
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

// A replayed read-only dispatch must stay read-only; the flag default would hand it write tools.
func TestApplyRecordRoundTripsReadOnly(t *testing.T) {
	s := newAskState()
	s.applyRecord(agent.Record{Agent: "claude", Request: agent.RunOptions{ReadOnly: true, Tools: true}})
	if !*s.readOnly || !*s.tools {
		t.Errorf("readOnly=%v tools=%v; want both on", *s.readOnly, *s.tools)
	}
	s.applyRecord(agent.Record{Agent: "claude", Request: agent.RunOptions{Tools: true}})
	if *s.readOnly {
		t.Error("readOnly stayed on for a record without it")
	}
}

// A failed job's stderr tail may carry a key; result must not echo it raw.
func TestResultExitRedactsError(t *testing.T) {
	const key = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	rc := resultExit(agent.Record{Status: "error", Error: "401 bad key " + key})
	os.Stderr = orig
	w.Close()
	out, _ := io.ReadAll(r)
	if rc != 1 || strings.Contains(string(out), key) {
		t.Errorf("rc=%d stderr=%q; want 1 and no raw key", rc, out)
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
	marker := gemini.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := vault.SetAgentRecord("gemini", vault.AgentRecord{Profile: "antigravity"}); err != nil { // no profile = off
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

// seedClaudeJob saves a finished claude record with a log and, when events is non-nil, an events file.
func seedClaudeJob(t *testing.T, at time.Time, events []agent.Event) agent.Record {
	t.Helper()
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "say hi"}, at)
	rec.Repo = t.TempDir()
	rec.Status, rec.SessionID, rec.DurationMs = "ok", "sess-1", 42
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec.LogPath(), []byte("hi there\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if events != nil {
		var b strings.Builder
		for _, ev := range events {
			line, _ := json.Marshal(ev)
			b.Write(line)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(rec.EventsPath(), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return rec
}

// TestAgentsJobLifecycleClaude: status/result/tail resolve a claude dispatch by id and read its events.
func TestAgentsJobLifecycleClaude(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env := agent.NewEnvelope(agent.Result{Agent: "claude", Answer: "from-event", SessionID: "sess-1"}, nil, "")
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)
	env.DispatchID = rec.ID
	evs := []agent.Event{
		{Schema: 1, Seq: 0, Type: agent.EventStart},
		{Schema: 1, Seq: 1, Type: agent.EventText, Text: "hi"},
		{Schema: 1, Seq: 2, Type: agent.EventResult, Envelope: &env},
	}
	var b strings.Builder
	for _, ev := range evs {
		line, _ := json.Marshal(ev)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(rec.EventsPath(), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	var code int
	out := captureStdout(t, func() { code = cmdAgentsJob("status", []string{rec.ID}) })
	if code != 0 || !strings.Contains(out, rec.ID) || !strings.Contains(out, "ok") {
		t.Errorf("status = %d %q, want 0 with id and ok", code, out)
	}

	out = captureStdout(t, func() { code = cmdAgentsJob("result", []string{rec.ID, "--format", "json"}) })
	var got agent.Envelope
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("result json = %d %q: %v", code, out, err)
	}
	if got.Answer != "from-event" || got.DispatchID != rec.ID {
		t.Errorf("envelope = %+v, want the result event's", got)
	}

	out = captureStdout(t, func() { code = cmdAgentsJob("tail", []string{rec.ID, "--format", "json", "-f"}) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 {
		t.Fatalf("tail = %d, %d lines %q, want 3 events then exit", code, len(lines), out)
	}
	for i, want := range []string{agent.EventStart, agent.EventText, agent.EventResult} {
		var ev agent.Event
		if err := json.Unmarshal([]byte(lines[i]), &ev); err != nil || ev.Type != want {
			t.Errorf("line %d = %q, want type %s", i, lines[i], want)
		}
	}
}

// TestAgentsResultJSONWithoutEvents: an old record with no events file rebuilds its envelope from record + log.
func TestAgentsResultJSONWithoutEvents(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)

	var code int
	out := captureStdout(t, func() { code = cmdAgentsJob("result", []string{rec.ID, "--format", "json"}) })
	var got agent.Envelope
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("result json = %d %q: %v", code, out, err)
	}
	if got.Answer != "hi there\n" || got.SessionID != "sess-1" || got.DispatchID != rec.ID || got.Agent != "claude" {
		t.Errorf("rebuilt envelope = %+v, want log answer, session and id from the record", got)
	}
}

// TestAgentsResultWarnsOnUnreadableEvents: an oversized event line warns on stderr and stdout stays a valid envelope.
func TestAgentsResultWarnsOnUnreadableEvents(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)
	if err := os.WriteFile(rec.EventsPath(), []byte(strings.Repeat("x", 17<<20)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := captureOutput(t, func() int { return cmdAgentsJob("result", []string{rec.ID, "--format", "json"}) })
	var got agent.Envelope
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || code != 0 {
		t.Fatalf("result json = %d %q: %v", code, stdout, err)
	}
	if got.Answer != "hi there\n" {
		t.Errorf("answer = %q, want the log rebuilt into the envelope", got.Answer)
	}
	if !strings.Contains(stderr, rec.ID) || !strings.Contains(stderr, "token too long") {
		t.Errorf("stderr = %q, want a warning naming the dispatch and the scan error", stderr)
	}
}

// TestAgentsCancelNotRunning: cancel on a finished record exits non-zero.
func TestAgentsCancelNotRunning(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)
	if code := cmdAgentsJob("cancel", []string{rec.ID}); code == 0 {
		t.Error("cancel of a finished job exited 0, want an error")
	}
}

const acmeProvider = `"providers":{"openai":{"acme":{"protocol":"openai","base_url":"https://acme.example/v1","auth":{"type":"none"}}}}`

// profileFixture isolates the vault and seeds provider acme (no opencodeID) plus, when active is set, a "work" set.
func profileFixture(t *testing.T, active string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	doc := `{` + acmeProvider + `}`
	if active != "" {
		doc = `{` + acmeProvider + `,"profiles":{"active":"` + active + `","sets":{"work":{
		  "codex":{"account":"acme","model":"acme-large","effort":"high"}}}}}`
	}
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "providers", doc) }); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
}

func TestClientPrepareHubNativeFallback(t *testing.T) {
	profileFixture(t, "")
	t.Setenv("HOME", t.TempDir())
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"ok"}}'`)
	client := newClient()
	for _, profile := range []string{"acme", "chatgpt"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: "opencode", Provider: profile, Prompt: "p", Fallback: router.FallbackAgents}, nil)
		if err != nil {
			t.Fatalf("Prepare(%s) = %v", profile, err)
		}
		res, err := client.Exec(context.Background(), p, nil, agent.RunRecordedOpts{})
		if len(res.Hops) != 1 {
			t.Fatalf("%s: want one dispatch, got %+v, err %v", profile, res.Hops, err)
		}
		if profile == "acme" && (err != nil || res.Answer != "ok") {
			t.Fatalf("native profile did not run: %+v, %v", res, err)
		}
		if profile == "chatgpt" && (err == nil || !strings.Contains(err.Error(), "opencode has no openai login")) {
			t.Fatalf("hub profile did not check its login: %+v, %v", res, err)
		}
	}
}

// A cancelled batch prepares nothing and never refreshes quota.
func TestClientPrepareHonorsCancellation(t *testing.T) {
	refreshes := 0
	prev := routeRefresh
	routeRefresh = func(context.Context, string) bool { refreshes++; return false }
	t.Cleanup(func() { routeRefresh = prev })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newClient().Prepare(ctx, agent.RunOptions{Agent: "opencode", Prompt: "p", Fallback: router.FallbackAgents}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if refreshes != 0 {
		t.Errorf("%d quota refreshes ran under a cancelled ctx", refreshes)
	}
}

// An explicit --profile with no --model runs the classifier's alias, as the pinned route does.
func TestClientPrepareProfileTakesTierSlot(t *testing.T) {
	profileFixture(t, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	client := newClient()
	for _, profile := range []string{"chatgpt", "acme"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: "opencode", Provider: profile, Prompt: "pong", Fallback: router.FallbackAgents}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, _ := client.Exec(context.Background(), p, nil, agent.RunRecordedOpts{})
		if len(res.Hops) != 1 {
			t.Fatalf("%s: hops = %+v, want one unavailable dispatch", profile, res.Hops)
		}
		if want := (router.RuleClassifier{}).Alias("pong"); res.Hops[0].Model != want {
			t.Errorf("%s: dispatch model = %q, want alias %q", profile, res.Hops[0].Model, want)
		}
	}
}

// An explicit --profile is one dispatch for every agent even with --fallback all: nothing can silently take over.
func TestClientPrepareProfileIsOneDispatchUnderFallbackAll(t *testing.T) {
	profileFixture(t, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	client := newClient()
	for _, name := range []string{"codex", "claude", "opencode", "copilot", "gemini"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: name, Provider: "acme", Prompt: "p", Fallback: router.FallbackAll}, nil)
		if err != nil {
			t.Fatalf("%s: Prepare = %v", name, err)
		}
		res, err := client.Exec(context.Background(), p, nil, agent.RunRecordedOpts{})
		if err == nil || len(res.Hops) != 1 {
			t.Errorf("%s: %d hops, err %v; want one failed dispatch without fallback", name, len(res.Hops), err)
		}
	}
}

// A batch task's own profile reaches the request and resolves to one dispatch, under the default --fallback all.
func TestBatchTaskProfileIsOneDispatch(t *testing.T) {
	profileFixture(t, "")
	line := batch.ParsedLine{N: 1, Task: batch.TaskLine{Prompt: "p", Profile: "acme"}}
	bt, err := batchTask(line, batchOpts{agent: "codex", fallback: router.FallbackAll}, agent.RunOptions{}, false, t.TempDir(), map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if bt.Req.Provider != "acme" {
		t.Fatalf("task request provider = %q, want acme", bt.Req.Provider)
	}
	p, err := bt.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	res, err := newClient().Exec(context.Background(), p, nil, agent.RunRecordedOpts{})
	if err == nil || len(res.Hops) != 1 {
		t.Fatalf("Exec: %d hops, err %v; want one unavailable dispatch", len(res.Hops), err)
	}
	bad := batch.ParsedLine{N: 2, Task: batch.TaskLine{Prompt: "p", Profile: "no-such-profile"}}
	bt, err = batchTask(bad, batchOpts{agent: "codex", fallback: router.FallbackAll}, agent.RunOptions{}, false, t.TempDir(), map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bt.Prepare(context.Background()); err == nil {
		t.Error("unknown batch profile prepared a chain, want an error before any run")
	}
}

// deadPID returns the pid of a child that has already exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("no true binary: %v", err)
	}
	return cmd.Process.Pid
}

func writeDispatch(t *testing.T, id, status string, pid int, age time.Duration) string {
	t.Helper()
	p := filepath.Join(paths.StateDir(), "dispatch", id+".json")
	rec := map[string]any{"id": id, "time": time.Now().Add(-age).UTC().Format(time.RFC3339Nano), "agent": "codex",
		"request": map[string]any{}, "status": status, "pid": pid, "repo": "/some/other/repo", "duration_ms": 3000}
	b, _ := json.Marshal(rec)
	writeFixtureFile(t, p, string(b))
	return p
}

func readDispatch(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return m
}

func TestCmdAgentsReapMarksLostDispatches(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dead := writeDispatch(t, "20261024T000001.000-codex", "running", deadPID(t), time.Hour)
	live := writeDispatch(t, "20261024T000002.000-codex", "running", os.Getpid(), time.Hour)
	done := writeDispatch(t, "20261024T000003.000-codex", "ok", deadPID(t), time.Hour)
	young := writeDispatch(t, "20261024T000004.000-codex", "running", 0, 5*time.Second)
	old0 := writeDispatch(t, "20261024T000005.000-codex", "running", 0, 10*time.Minute)

	code, stdout, stderr := captureOutput(t, func() int { return cmdAgents([]string{"reap"}) })
	if code != 0 {
		t.Fatalf("reap exit %d, stderr=%s", code, stderr)
	}
	for _, p := range []string{dead, old0} {
		m := readDispatch(t, p)
		if m["status"] != "error" || m["error"] != "lost: process gone" {
			t.Errorf("%s: status/error = %v/%v, want error/lost: process gone", filepath.Base(p), m["status"], m["error"])
		}
		if m["duration_ms"] != float64(3000) || m["agent"] != "codex" {
			t.Errorf("%s: other fields not preserved: %v", filepath.Base(p), m)
		}
	}
	for _, p := range []string{live, young} {
		if m := readDispatch(t, p); m["status"] != "running" {
			t.Errorf("%s: alive/in-grace record was reaped: %v", filepath.Base(p), m["status"])
		}
	}
	if m := readDispatch(t, done); m["status"] != "ok" || m["error"] != nil {
		t.Errorf("finished record touched: %v", m)
	}
	if !strings.Contains(stdout, "000001") {
		t.Errorf("reap output should name the reaped dispatch, got %q", stdout)
	}
}

func TestCmdAgentsReapDryRunChangesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dead := writeDispatch(t, "20261024T000001.000-codex", "running", deadPID(t), time.Hour)
	dir := filepath.Join(paths.StateDir(), "agents-inflight")
	marker := filepath.Join(dir, "agent-"+strconv.Itoa(deadPID(t))+"-toolu_dead")
	writeFixtureFile(t, marker, "")
	before, _ := os.ReadFile(dead)

	code, stdout, stderr := captureOutput(t, func() int { return cmdAgents([]string{"reap", "--dry-run"}) })
	if code != 0 {
		t.Fatalf("reap --dry-run exit %d, stderr=%s", code, stderr)
	}
	if after, _ := os.ReadFile(dead); string(after) != string(before) {
		t.Errorf("--dry-run rewrote the record:\n%s", after)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("--dry-run removed a marker: %v", err)
	}
	if !strings.Contains(stdout, "000001") {
		t.Errorf("--dry-run should still print what it would change, got %q", stdout)
	}
}

func TestCmdAgentsReapMarkers(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := filepath.Join(paths.StateDir(), "agents-inflight")
	deadM := filepath.Join(dir, "agent-"+strconv.Itoa(deadPID(t))+"-toolu_dead")
	liveM := filepath.Join(dir, "agent-"+strconv.Itoa(os.Getpid())+"-toolu_live")
	lock := filepath.Join(dir, ".lock")
	stray := filepath.Join(dir, "notes.txt")
	for _, p := range []string{deadM, liveM, lock, stray} {
		writeFixtureFile(t, p, "")
	}
	if code, _, stderr := captureOutput(t, func() int { return cmdAgents([]string{"reap"}) }); code != 0 {
		t.Fatalf("reap exit %d, stderr=%s", code, stderr)
	}
	if _, err := os.Stat(deadM); !os.IsNotExist(err) {
		t.Errorf("dead-owner marker should be removed (err=%v)", err)
	}
	for _, p := range []string{liveM, lock, stray} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must survive reap: %v", filepath.Base(p), err)
		}
	}
}

func TestCmdAgentsReapNothingToDo(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if code, _, stderr := captureOutput(t, func() int { return cmdAgents([]string{"reap"}) }); code != 0 {
		t.Fatalf("reap on empty state exit %d, stderr=%s", code, stderr)
	}
}

// agents tree --running lists running dispatches machine-wide, including other repos, ignoring --days.
func TestCmdAgentsTreeRunningListsDispatchesMachineWide(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeDispatch(t, "20261024T000002.000-codex", "running", os.Getpid(), 72*time.Hour)
	writeDispatch(t, "20261024T000003.000-codex", "ok", os.Getpid(), time.Minute)
	code, stdout, stderr := captureOutput(t, func() int { return cmdAgents([]string{"tree", "--days", "1"}) })
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "000002") {
		t.Errorf("running dispatch from another repo missing:\n%s", stdout)
	}
	if strings.Contains(stdout, "000003") {
		t.Errorf("finished dispatch listed by default:\n%s", stdout)
	}
}

func TestAgentsUsageFits80(t *testing.T) {
	usage := renderHelpPage(agentsPage)
	for line := range strings.SplitSeq(usage, "\n") {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("agentsUsage row is %d runes (>80): %q", n, line)
		}
	}
	if !strings.Contains(usage, "  reap") {
		t.Error("agentsPage must document reap")
	}
	code, _, stderr := captureOutput(t, func() int { return cmdAgents([]string{"reap", "-h"}) })
	if code != 0 || !strings.Contains(stderr, "--dry-run") {
		t.Errorf("agents reap help must document --dry-run: exit %d:\n%s", code, stderr)
	}
}

// --background and -bg are a superset of the default: a running dispatch from another repo stays listed.
func TestCmdAgentsTreeBackgroundKeepsRunningDispatches(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeDispatch(t, "20261024T000002.000-codex", "running", os.Getpid(), 72*time.Hour)
	for _, flag := range []string{"--background", "-bg"} {
		code, stdout, stderr := captureOutput(t, func() int { return cmdAgents([]string{"tree", flag, "--days", "1"}) })
		if code != 0 {
			t.Fatalf("%s: exit %d, stderr=%s", flag, code, stderr)
		}
		if !strings.Contains(stdout, "000002") {
			t.Errorf("%s dropped the running dispatch:\n%s", flag, stdout)
		}
	}
}

// notAVendor is an unregistered vendor name: every vendoragent lookup and agent.Vendor's own lookup
// reject it in-process, fast, so dispatch routing can be probed without touching a real vendor binary.
const notAVendor = "zzz-not-a-vendor"

// TestVendorUsageFits80: every rendered line stays within an 80-column terminal, and gemini's block
// names its extra bridge verb (contract.md's help-text target).
func TestVendorUsageFits80(t *testing.T) {
	for _, vendor := range cliAgentNames() {
		usage := renderHelpPage(agentPage(vendor))
		for line := range strings.SplitSeq(strings.TrimRight(usage, "\n"), "\n") {
			if n := utf8.RuneCountInString(line); n > 80 {
				t.Errorf("vendorUsage(%q) line %q is %d runes, want <= 80", vendor, line, n)
			}
		}
		if vendor == "gemini" && !strings.Contains(usage, "bridge") {
			t.Errorf("vendorUsage(gemini) missing the bridge row:\n%s", usage)
		}
	}
}

// TestVendorVerbDoubleDashForcesPassthrough: `<v> -- status` must reach launchVendor, not
// cmdVendorStatus, even though "status" is also a subverb word — distinguished here by message
// shape: launchVendor's failure is agent.Vendor's generic "unknown agent" text, never the
// "<v> status: ..." prefix cmdVendorStatus would produce.
func TestVendorVerbDoubleDashForcesPassthrough(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return AgentVerb(notAVendor, []string{"--", "status"}) })
	if strings.Contains(stderr, notAVendor+" status:") {
		t.Errorf("-- status reached cmdVendorStatus instead of launchVendor: %q", stderr)
	}
	if !strings.Contains(stderr, "unknown agent") {
		t.Errorf("-- status stderr = %q, want agent.Vendor's \"unknown agent\" message", stderr)
	}
}

// TestVendorVerbRoutesStatusResultCancelToCmdVendorSub: these three subverbs reach their
// cmdVendorSub handlers, proven by the handler-specific "<v> <verb>: ..." error prefix that only
// appears once vendoragent's own unknown-vendor error is wrapped by that handler.
func TestVendorVerbRoutesStatusResultCancelToCmdVendorSub(t *testing.T) {
	for _, sub := range []string{"status", "result", "cancel", "tail"} {
		_, _, stderr := captureOutput(t, func() int { return AgentVerb(notAVendor, []string{sub}) })
		want := notAVendor + " " + sub + ":"
		if !strings.Contains(stderr, want) {
			t.Errorf("vendorVerb(%q, [%q]) stderr = %q, want it to contain %q", notAVendor, sub, stderr, want)
		}
	}
}

// TestVendorVerbRoutesSetupToCmdVendorSetup: cmdVendorSetup prints its report to stdout (not an
// error), so unlike the other handlers its routing proof reads stdout, keyed on rep.Vendor appearing
// as the row's first column.
func TestVendorVerbRoutesSetupToCmdVendorSetup(t *testing.T) {
	_, stdout, _ := captureOutput(t, func() int { return AgentVerb(notAVendor, []string{"setup"}) })
	if !strings.Contains(stdout, notAVendor+"  agent  fail") {
		t.Errorf("vendorVerb(%q, [setup]) stdout = %q, want cmdVendorSetup's report row", notAVendor, stdout)
	}
}

// catalog reaches cmdVendorModels, not launchVendor, with canonical diagnostics and help.
func TestVendorVerbRoutesCatalogToCmdVendorModels(t *testing.T) {
	for _, sub := range []string{"catalog"} {
		code, _, stderr := captureOutput(t, func() int { return AgentVerb("codex", []string{sub, "unexpected"}) })
		if code != 2 || !strings.Contains(stderr, "codex catalog: unexpected argument") {
			t.Errorf("vendorVerb(%s): code=%d stderr=%q", sub, code, stderr)
		}
		code, _, stderr = captureOutput(t, func() int { return AgentVerb("codex", []string{sub, "--help"}) })
		if code != 0 || !strings.HasPrefix(stderr, "usage: claude-code codex catalog") {
			t.Errorf("vendorVerb(%s --help): code=%d stderr=%q", sub, code, stderr)
		}
	}
}

// TestVendorVerbRoutesResumeToCmdVendorResume: `resume` with no id hits cmdVendorResume's own usage
// check before ever reaching cmdAsk (which would otherwise dispatch a real agent run).
func TestVendorVerbRoutesResumeToCmdVendorResume(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return AgentVerb("codex", []string{"resume"}) })
	if !strings.Contains(stderr, "codex resume: usage:") {
		t.Errorf("vendorVerb(resume, no id) stderr = %q, want cmdVendorResume's usage message", stderr)
	}
}

// TestVendorVerbBareLaunchesEvenWhenFirstWordLooksLikeASubverb: an unknown bare word (not "--", not
// "ask", not in vendorSubverbs) falls through to launchVendor with args forwarded unchanged.
func TestVendorVerbBareLaunchesEvenWhenFirstWordLooksLikeASubverb(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return AgentVerb(notAVendor, []string{"--native-flag", "x"}) })
	if !strings.Contains(stderr, "unknown agent") {
		t.Errorf("bare launch stderr = %q, want agent.Vendor's unknown-vendor message", stderr)
	}
}

// TestVendorVerbHelpFlag checks that vendor help uses the shared page and exits successfully.
func TestVendorVerbHelpFlag(t *testing.T) {
	for _, flagArg := range []string{"-h", "--help", "help"} {
		code, _, stderr := captureOutput(t, func() int { return AgentVerb("codex", []string{flagArg}) })
		if code != 0 {
			t.Errorf("vendorVerb(codex, %q) exit %d, want 0", flagArg, code)
		}
		if !strings.HasPrefix(stderr, "usage: claude-code codex") {
			t.Errorf("vendorVerb(codex, %q) stderr missing usage header: %q", flagArg, stderr)
		}
		for _, want := range []string{"  catalog", "select the model for this launch"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("vendorVerb(codex, %q) help missing %q: %s", flagArg, want, stderr)
			}
		}
	}
}

// TestVendorModelsAliasRemoved: models is no longer a subverb; it falls through to the agent launch like any bare word.
func TestVendorModelsAliasRemoved(t *testing.T) {
	if agentSubverbs["models"] {
		t.Error("models must not route to cmdVendorSub")
	}
}

// TestVendorSubverbsCoverContractVerbs pins vendorSubverbs against contract.md's core verb set — a
// renamed or dropped subverb here silently stops routing. Checks presence, not exact membership, so
// a later addition (e.g. explore) doesn't need a second hand-copied list kept in lockstep with
// TestSpecTreeSubwordsMatchDispatch's own derivation off the same map.
func TestVendorSubverbsCoverContractVerbs(t *testing.T) {
	for _, w := range []string{"resume", "status", "result", "cancel", "tail", "setup", "catalog", "review", "transfer"} {
		if !agentSubverbs[w] {
			t.Errorf("vendorSubverbs missing %q", w)
		}
	}
}

// TestVendorCompletionOffersEverySubverb: tab-completing a vendor's own bare position (`codex ⇥`)
// offers "ask" plus every word in vendorSubverbs, for every vendor — derived off vendorSubverbs so a
// future addition (e.g. explore) is picked up without a second hand-copied list to keep in lockstep.
func TestVendorCompletionOffersEverySubverb(t *testing.T) {
	want := []string{"ask"}
	for sub := range agentSubverbs {
		want = append(want, sub)
	}
	slices.Sort(want)
	for _, vendor := range cliAgentNames() {
		got := completionCandidates([]string{vendor, ""})
		slices.Sort(got)
		if vendor == "gemini" {
			got = slices.DeleteFunc(slices.Clone(got), func(s string) bool { return s == "bridge" })
		}
		if !slices.Equal(got, want) {
			t.Errorf("completionCandidates([%q \"\"]) = %v, want %v", vendor, got, want)
		}
	}
}

// TestVendorCompletionSubverbFlags: a vendor subverb's own flags complete past it, pinning two
// representative cases (status's --all and setup's --format) against their flagInfo wiring.
func TestVendorCompletionSubverbFlags(t *testing.T) {
	if got := completionCandidates([]string{"codex", "status", "--"}); !slices.Contains(got, "--all") {
		t.Errorf("codex status -- = %v, missing --all", got)
	}
	if got := completionCandidates([]string{"codex", "setup", "--"}); !slices.Contains(got, "--format") {
		t.Errorf("codex setup -- = %v, missing --format", got)
	}
}

// TestVendorCompletionDoesNotFollowDoubleDash: `<v> -- ⇥` is past the vendor's own (unknown) native
// flags — it must offer nothing, not the subverb domain again (mirrors the old `run codex --` guard).
func TestVendorCompletionDoesNotFollowDoubleDash(t *testing.T) {
	if got := completionCandidates([]string{"codex", "--", ""}); len(got) != 0 {
		t.Errorf("codex -- ⇥ = %v, want none (codex's own flags are not ours to know)", got)
	}
}

func TestVendorUsageBridgeRowOnlyForGemini(t *testing.T) {
	if u := renderHelpPage(agentPage("codex")); strings.Contains(u, "bridge") {
		t.Errorf("vendorUsage(codex) must not carry the bridge row:\n%s", u)
	}
	if u := renderHelpPage(agentPage("gemini")); !strings.Contains(u, "  bridge") {
		t.Errorf("vendorUsage(gemini) missing the bridge row:\n%s", u)
	}
}

func TestPrintDispatchLogPreviewCutsOnRuneBoundary(t *testing.T) {
	// Given a prompt of 4-byte runes longer than the preview; When the log prints; Then the cut line is valid UTF-8
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.NewRecord("codex", agent.RunOptions{Prompt: strings.Repeat("😀", 100)}, time.Now())
	rec.Status = "ok"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	_, out, _ := captureOutput(t, func() int { return printDispatchLog("text") })
	if !utf8.ValidString(out) {
		t.Fatalf("output is not valid UTF-8: %q", out)
	}
	if !strings.Contains(out, "😀…") {
		t.Errorf("output lacks a truncated preview ending in …:\n%s", out)
	}
}

func saveResultRecord(t *testing.T, status, errMsg string) agent.Record {
	t.Helper()
	cwd, _ := os.Getwd()
	rec := agent.NewRecord("codex", agent.RunOptions{Prompt: "p"}, time.Now())
	rec.Status, rec.Error = status, errMsg
	rec.Repo = agent.RepoRoot(context.Background(), cwd)
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestVendorResultErrorRecordExits1(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := saveResultRecord(t, "error", "boom happened")
	for _, args := range [][]string{{rec.ID}, {"--format", "json", rec.ID}} {
		code, _, stderr := captureOutput(t, func() int { return agentResult("codex", args) })
		if code != 1 || !strings.Contains(stderr, "error: boom happened") {
			t.Errorf("args %v: exit %d stderr %q, want 1 and \"error: boom happened\"", args, code, stderr)
		}
	}
}

func TestVendorResultOKRecordExits0(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := saveResultRecord(t, "ok", "")
	code, _, stderr := captureOutput(t, func() int { return agentResult("codex", []string{rec.ID}) })
	if code != 0 || strings.Contains(stderr, "error:") {
		t.Errorf("exit %d stderr %q, want 0 and no error line", code, stderr)
	}
}

// seedTailJob saves a finished codex record whose log holds body and returns its id.
func seedTailJob(t *testing.T, status, body string) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	logPath := filepath.Join(cwd, "job.log")
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := agent.NewRecord("codex", agent.RunOptions{Prompt: "tail me"}, time.Now())
	rec.Repo, rec.Status, rec.Log = cwd, status, logPath
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

// TestVendorTailPrintsFinishedLog: tail <id> on a finished ok job prints its log and exits 0.
func TestVendorTailPrintsFinishedLog(t *testing.T) {
	id := seedTailJob(t, "ok", "line one\nline two\n")
	code, stdout, stderr := captureOutput(t, func() int { return AgentVerb("codex", []string{"tail", id}) })
	if code != 0 || stdout != "line one\nline two\n" {
		t.Errorf("tail ok job = exit %d stdout %q stderr %q, want exit 0 with the log verbatim", code, stdout, stderr)
	}
}

// TestVendorTailErrorJobExitsOne: a job that ended in error still prints its log but exits 1.
func TestVendorTailErrorJobExitsOne(t *testing.T) {
	id := seedTailJob(t, "error", "boom\n")
	code, stdout, _ := captureOutput(t, func() int { return AgentVerb("codex", []string{"tail", id}) })
	if code != 1 || !strings.Contains(stdout, "boom") {
		t.Errorf("tail error job = exit %d stdout %q, want exit 1 with the log", code, stdout)
	}
}

// TestVendorTailUnknownIDFails: an id matching no job errors with the "<v> tail:" prefix, exit 1.
func TestVendorTailUnknownIDFails(t *testing.T) {
	seedTailJob(t, "ok", "x\n")
	code, _, stderr := captureOutput(t, func() int { return AgentVerb("codex", []string{"tail", "zzzzzzzz"}) })
	if code != 1 || !strings.Contains(stderr, "codex tail:") {
		t.Errorf("tail unknown id = exit %d stderr %q, want exit 1 and the \"codex tail:\" prefix", code, stderr)
	}
}

// TestVendorUsageNamesTail: every vendor's help block carries a tail row.
func TestVendorUsageNamesTail(t *testing.T) {
	for _, vendor := range cliAgentNames() {
		if !strings.Contains(renderHelpPage(agentPage(vendor)), "tail") {
			t.Errorf("vendorUsage(%q) has no tail row", vendor)
		}
	}
}
