package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/batch"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

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
		{name: "fresh --keep-env is recorded", flags: []string{"--keep-env", "LANG,HOME"},
			wantTimeout: 10 * time.Minute, wantKeepEnv: []string{"LANG", "HOME"}},
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
	// Mirror job.Spawn: the job child's own stdout and stderr are the log file.
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

// hostFixture installs a fake ssh (drops options+host, runs the decoded command via sh) on PATH, hermetic dirs, and
// a fake remote bin that prints banner then the given NDJSON lines; it returns the remote bin path.
func hostFixture(t *testing.T, banner string, lines []string, exit int) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	sshBody := "#!/bin/sh\nwhile [ \"$1\" != \"--\" ]; do shift; done\nshift; shift\nexec sh -c \"$1\"\n"
	if exit == 255 {
		sshBody = "#!/bin/sh\necho 'ssh: connect to host fakehost port 22: Connection refused' >&2\nexit 255\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(sshBody), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "events.ndjson")
	if err := os.WriteFile(data, []byte(strings.Join(lines, "\n")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "remote-cc")
	body := "#!/bin/sh\n"
	if banner != "" {
		body += "echo '" + banner + "'\n"
	}
	body += "cat '" + data + "'\n"
	if exit != 0 && exit != 255 {
		body += "exit " + string(rune('0'+exit)) + "\n"
	}
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

func ndjson(t *testing.T, evs ...agent.Event) []string {
	t.Helper()
	var out []string
	for i, ev := range evs {
		ev.Schema, ev.Seq, ev.DispatchID = agent.EventSchema, int64(i+1), "REMOTE-ID"
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

func resultEvent(env agent.Envelope) agent.Event {
	env.Schema = agent.EnvelopeSchema
	return agent.Event{Type: agent.EventResult, Host: "remotebox", Envelope: &env}
}

// ask --host over a fake ssh: the remote start is dropped, events keep the remote host stamp, the local result carries
// the remote envelope's cost/structured/emulated/session plus local drops, and the banner goes to the dispatch log.
func TestAskHostRelaysRemoteStream(t *testing.T) {
	env := agent.Envelope{Status: agent.StatusOK, Agent: "claude", Answer: "hi there", SessionID: "sess-9",
		Usage:      &agent.Usage{InputTokens: 3, OutputTokens: 4, CostUSD: 0.0123},
		Structured: json.RawMessage(`{"k":1}`), Emulated: []string{"--output-schema"}, Dropped: []string{"--effort"}}
	lines := ndjson(t,
		agent.Event{Type: agent.EventStart, Host: "remotebox"},
		agent.Event{Type: agent.EventText, Host: "remotebox", Text: "hi there"},
		agent.Event{Type: agent.EventToolUse, Host: "remotebox", Tool: &agent.ToolRef{ID: "t1", Name: "Read"}},
		resultEvent(env))
	bin := hostFixture(t, "Welcome to fakehost banner", lines, 0)
	att := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(att, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out := captureAskStdout(t, "--host", "fakehost", "--remote-bin", bin, "--format", "stream-json",
		"--attach", att, "--keep-env", "FOO", "do it")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "banner") {
		t.Fatalf("banner leaked to stdout:\n%s", out)
	}
	evs := decodeEvents(t, out)
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	if got, want := strings.Join(types, ","), "start,text,tool_use,result"; got != want {
		t.Fatalf("types = %s, want %s", got, want)
	}
	local := evs[0].DispatchID
	if local == "" || local == "REMOTE-ID" {
		t.Fatalf("start must carry the local dispatch id, got %q", local)
	}
	for _, ev := range evs[1:3] {
		if ev.Host != "remotebox" {
			t.Errorf("%s lost the remote host stamp: %q", ev.Type, ev.Host)
		}
		if ev.DispatchID != local {
			t.Errorf("%s dispatch_id %q, want local %q", ev.Type, ev.DispatchID, local)
		}
	}
	got := evs[3].Envelope
	if got == nil || got.Usage == nil || got.Usage.CostUSD != 0.0123 || string(got.Structured) != `{"k":1}` ||
		got.SessionID != "sess-9" || got.Answer != "hi there" || got.DispatchID != local {
		t.Fatalf("result envelope lost remote fields: %+v", got)
	}
	if !slices.Contains(got.Emulated, "--output-schema") {
		t.Errorf("emulated = %v", got.Emulated)
	}
	for _, d := range []string{"--attach", "--keep-env", "--effort"} {
		if !slices.Contains(got.Dropped, d) {
			t.Errorf("dropped %v lacks %s", got.Dropped, d)
		}
	}
	logb, err := os.ReadFile(filepath.Join(paths.DispatchDir(), local+".log"))
	if err != nil || !strings.Contains(string(logb), "Welcome to fakehost banner") {
		t.Fatalf("banner not in dispatch log (err %v):\n%s", err, logb)
	}
}

func lastEnvelope(t *testing.T, out string) *agent.Envelope {
	t.Helper()
	evs := decodeEvents(t, out)
	e := evs[len(evs)-1]
	if e.Type != agent.EventResult || e.Envelope == nil {
		t.Fatalf("no result event last:\n%s", out)
	}
	return e.Envelope
}

func TestAskHostErrorClassification(t *testing.T) {
	cases := []struct {
		name, kind, msg string
		exit            int
		lines           func(*testing.T) []string
	}{
		{"ssh 255", "transient", "connection failed", 255, func(*testing.T) []string { return nil }},
		{"exit without result", "fatal", "stream-json", 3, func(*testing.T) []string { return nil }},
		{"remote schema error", "schema", "remote fakehost:", 0, func(t *testing.T) []string {
			return ndjson(t, resultEvent(agent.Envelope{Status: agent.StatusError, Agent: "claude",
				Error: &agent.EnvelopeError{Kind: "schema", Message: "answer fails the schema"}}))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var lines []string
			if c.lines != nil {
				lines = c.lines(t)
			}
			bin := hostFixture(t, "", lines, c.exit)
			code, out := captureAskStdout(t, "--host", "fakehost", "--remote-bin", bin, "--format", "stream-json", "go")
			if code == 0 {
				t.Fatalf("want a failure exit:\n%s", out)
			}
			env := lastEnvelope(t, out)
			if env.Error == nil || env.Error.Kind != c.kind || !strings.Contains(env.Error.Message, c.msg) {
				t.Fatalf("want kind %s containing %q, got %+v", c.kind, c.msg, env.Error)
			}
		})
	}
}

func TestAskHostRefusesLocalOnlyModes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for _, extra := range [][]string{{"--background"}, {"--job", "abc"}, {"--batch", "-"}} {
		args := append([]string{"--host", "fakehost", "--format", "json"}, extra...)
		code, out := captureAskStdout(t, append(args, "task")...)
		if code == 0 || !strings.Contains(out, "--host cannot be combined") {
			t.Errorf("%v: code %d, out %q", extra, code, out)
		}
	}
}

// recordingRemote wraps the fixture's remote bin so each invocation appends its argv line to the returned file.
func recordingRemote(t *testing.T, bin string) string {
	t.Helper()
	calls := filepath.Join(t.TempDir(), "calls")
	inner := bin + ".inner"
	if err := os.Rename(bin, inner); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\nexec '" + inner + "' \"$@\"\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return calls
}

func readCalls(t *testing.T, calls string) []string {
	t.Helper()
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// The caller's --fallback reaches the remote ask instead of a hard-coded none.
func TestAskHostForwardsFallback(t *testing.T) {
	lines := ndjson(t, resultEvent(agent.Envelope{Status: agent.StatusOK, Agent: "claude", Answer: "ok"}))
	bin := hostFixture(t, "", lines, 0)
	calls := recordingRemote(t, bin)
	if code, out := captureAskStdout(t, "--host", "fakehost", "--remote-bin", bin, "--fallback", "agents",
		"--format", "json", "go"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := readCalls(t, calls); !strings.Contains(got[0], "--fallback agents") {
		t.Fatalf("remote argv %q lacks --fallback agents", got[0])
	}
}

// A rejected --env must never echo its value: the flag parser prints the value of a failing Set.
func TestAskEnvRejectionNeverEchoesValue(t *testing.T) {
	for _, arg := range []string{"ANTHROPIC_API_KEY=supersecret", "HARNESS_VAULT_KEY=supersecret", "supersecret"} {
		code, stdout, stderr := captureOutput(t, func() int { return runAsk([]string{"--env", arg, "hi"}, false) })
		if code == 0 || strings.Contains(stdout+stderr, "supersecret") {
			t.Errorf("--env %s: code=%d, value leaked or accepted:\nstdout=%q\nstderr=%q", arg, code, stdout, stderr)
		}
	}
}

// ask --retry of a --host dispatch replays it on the same host through the same remote binary, never locally.
func TestAskRetryOfHostDispatchRunsRemote(t *testing.T) {
	lines := ndjson(t, resultEvent(agent.Envelope{Status: agent.StatusOK, Agent: "claude", Answer: "ok"}))
	bin := hostFixture(t, "", lines, 0)
	calls := recordingRemote(t, bin)
	code, out := captureAskStdout(t, "--host", "fakehost", "--remote-bin", bin, "--format", "json", "go")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var env agent.Envelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.DispatchID == "" {
		t.Fatalf("no dispatch id (%v):\n%s", err, out)
	}
	if code, out = captureAskStdout(t, "--retry", env.DispatchID, "--format", "json"); code != 0 {
		t.Fatalf("retry exit %d:\n%s", code, out)
	}
	if got := readCalls(t, calls); len(got) != 2 {
		t.Fatalf("remote ran %d times, want 2 (the retry ran locally): %q", len(got), got)
	}
}

// ask --retry keeps the original --fallback policy instead of reverting to the all default.
func TestAskRetryKeepsFallbackPolicy(t *testing.T) {
	lines := ndjson(t, resultEvent(agent.Envelope{Status: agent.StatusOK, Agent: "claude", Answer: "ok"}))
	bin := hostFixture(t, "", lines, 0)
	calls := recordingRemote(t, bin)
	code, out := captureAskStdout(t, "--host", "fakehost", "--remote-bin", bin, "--fallback", "none", "--format", "json", "go")
	var env agent.Envelope
	if err := json.Unmarshal([]byte(out), &env); code != 0 || err != nil {
		t.Fatalf("exit %d (%v):\n%s", code, err, out)
	}
	if code, out = captureAskStdout(t, "--retry", env.DispatchID, "--format", "json"); code != 0 {
		t.Fatalf("retry exit %d:\n%s", code, out)
	}
	if got := readCalls(t, calls); len(got) != 2 || !strings.Contains(got[1], "--fallback none") {
		t.Fatalf("retry argv %q, want --fallback none", got)
	}
}

// A failure after selector resolution publishes the resolved agent, not the raw --agents key.
func TestAskFailureEnvelopeCarriesResolvedRunner(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	defs := `{"sec":{"prompt":"Be strict.","agent":"codex"}}`
	args := []string{"--agents", defs, "--agent", "sec", "--format", "json", "--background", "--env", "A=b", "x"}
	code, stdout, stderr := captureOutput(t, func() int { return cmdAsk(args) })
	var env struct{ Agent string }
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || code != 2 {
		t.Fatalf("exit %d, stdout %q, err %v, stderr %q; want a usage envelope", code, stdout, err, stderr)
	}
	if env.Agent != "codex" {
		t.Fatalf("envelope agent = %q, want the resolved agent codex", env.Agent)
	}
}

// captureAskStdout runs cmdAsk and returns its exit code and stdout.
func captureAskStdout(t *testing.T, args ...string) (int, string) {
	t.Helper()
	code, stdout, _ := captureOutput(t, func() int { return cmdAsk(args) })
	return code, stdout
}

func decodeEvents(t *testing.T, s string) []agent.Event {
	t.Helper()
	var evs []agent.Event
	for l := range strings.SplitSeq(strings.TrimSpace(s), "\n") {
		var ev agent.Event
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("line %q is not an event: %v", l, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

// ask --format stream-json over a fake claude: ordered start..text..tool_use..result, seq rising, the result
// envelope answer equals the streamed text, and the on-disk events log mirrors stdout with secrets redacted.
func TestAskStreamJSONSpansEventsAndLog(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	secret := "ghp_" + strings.Repeat("a", 36)
	pathstest.FakeVendor(t, "claude", "cat > /dev/null\n"+
		`printf '%s\n' '{"type":"assistant","session_id":"s1","message":{"model":"m","content":[{"type":"text","text":"hello "},{"type":"tool_use","id":"tu1","name":"Read"}]}}'`+"\n"+
		`printf '%s\n' '{"type":"assistant","parent_tool_use_id":"tu1","message":{"model":"m","content":[{"type":"text","text":"SUBAGENT"}]}}'`+"\n"+
		`printf '%s\n' '{"type":"assistant","session_id":"s1","message":{"model":"m","content":[{"type":"text","text":"token `+secret+`"}]}}'`+"\n"+
		`printf '%s\n' '{"type":"result","subtype":"success","session_id":"s1","result":"hello \ntoken `+secret+`"}'`+"\n")

	code, out := captureAskStdout(t, "--format", "stream-json", "--fallback", "none", "say hi")
	if code != 0 {
		t.Fatalf("exit %d; stdout:\n%s", code, out)
	}
	evs := decodeEvents(t, out)
	var types []string
	var text strings.Builder
	for i, ev := range evs {
		types = append(types, ev.Type)
		if ev.Type == agent.EventText {
			if ev.Sep {
				text.WriteString("\n")
			}
			text.WriteString(ev.Text)
		}
		if i > 0 && ev.Seq <= evs[i-1].Seq {
			t.Errorf("seq not strictly increasing at %d: %d after %d", i, ev.Seq, evs[i-1].Seq)
		}
	}
	if want := "start,text,tool_use,text,result"; strings.Join(types, ",") != want {
		t.Fatalf("types = %v, want %s", types, want)
	}
	if strings.Contains(text.String(), "SUBAGENT") {
		t.Errorf("a parent_tool_use_id turn leaked into text events: %q", text.String())
	}
	res := evs[len(evs)-1]
	if res.Envelope == nil || res.Envelope.Answer != text.String() {
		t.Fatalf("result envelope answer = %+v, concatenated text = %q", res.Envelope, text.String())
	}
	if res.DispatchID == "" || res.Envelope.DispatchID != res.DispatchID {
		t.Errorf("dispatch ids: result %q, envelope %+v", res.DispatchID, res.Envelope)
	}

	logb, err := os.ReadFile(filepath.Join(paths.DispatchDir(), res.DispatchID+".events.jsonl"))
	if err != nil {
		t.Fatalf("events log missing: %v", err)
	}
	logged := decodeEvents(t, string(logb))
	if len(logged) != len(evs) {
		t.Fatalf("events log has %d events, stdout %d", len(logged), len(evs))
	}
	for i := range logged {
		if logged[i].Type != evs[i].Type {
			t.Errorf("log event %d type %s, stdout %s", i, logged[i].Type, evs[i].Type)
		}
	}
	if strings.Contains(string(logb), secret) || !strings.Contains(string(logb), "REDACTED") {
		t.Errorf("events log must redact the secret:\n%s", logb)
	}
}

// A failure before any run (unknown model route, missing binary) prints exactly one result event.
func TestAskStreamJSONPreRunFailureOneResultEvent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	code, out := captureAskStdout(t, "--format", "stream-json", "--agent", "nosuchagent", "hi")
	if code == 0 {
		t.Fatalf("want a failing exit; stdout:\n%s", out)
	}
	evs := decodeEvents(t, out)
	if len(evs) != 1 || evs[0].Type != agent.EventResult || evs[0].Envelope == nil || evs[0].Envelope.Status == "" {
		t.Fatalf("want exactly one result event with an envelope, got %d: %s", len(evs), out)
	}
}

// With --stdin-framed the prompt ends at a NUL; stdin closing after that cancels a run that is still blocked.
func TestAskStdinFramedCancelsOnEOF(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ready := filepath.Join(t.TempDir(), "ready")
	pathstest.FakeVendor(t, "claude", "cat > /dev/null\ntouch "+ready+"\nexec sleep 60\n")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn := os.Stdin
	os.Stdin = pr
	defer func() { os.Stdin = oldIn }()
	if _, err := pw.WriteString("do it\x00"); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		code int
		out  string
	}
	done := make(chan outcome, 1)
	go func() {
		code, out := captureAskStdout(t, "--format", "json", "--fallback", "none", "--stdin-framed")
		done <- outcome{code, out}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case o := <-done:
			t.Fatalf("run ended before stdin closed: %+v", o)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("fake claude never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	pw.Close()
	select {
	case o := <-done:
		if o.code == 0 || !strings.Contains(o.out, `"kind":"canceled"`) {
			t.Fatalf("want a canceled failure, got code %d:\n%s", o.code, o.out)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("cancel took %s", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run survived stdin EOF")
	}
}

// --agent vendor:persona loads agents/<name>.md or a path into the logged request, and --retry replays it without the flag.
func TestAskPersonaLoadsAndRetryKeepsIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir) // no agent binary: the run fails after the record is saved
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	def := "---\nname: tester\ndescription: Tests things\ntools: Read, Grep\nmodel: sonnet\n---\nYou test.\n"
	if err := os.WriteFile(filepath.Join(dir, "agents", "tester.md"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.md")
	if err := os.WriteFile(other, []byte("---\ndescription: By path\n---\nYou are by path.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ask := func(args ...string) agent.RunOptions {
		t.Helper()
		if code, _, stderr := captureOutput(t, func() int { return cmdAsk(args) }); code != 1 {
			t.Fatalf("exit = %d, want 1 for unavailable agent; stderr=%s", code, stderr)
		}
		return agent.RecentRecords(1)[0].Request
	}
	base := []string{"--fallback", "none", "--format", "json"}
	p := ask(append(base, "--agent", "claude:tester", "go")...).Persona
	if p == nil || p.Name != "tester" || p.Description != "Tests things" || p.Prompt != "You test." || p.Model != "sonnet" ||
		len(p.Tools) != 2 || p.Tools[0] != "Read" {
		t.Fatalf("named persona = %+v", p)
	}
	if p := ask(append(base, "--agent", "claude:"+other, "go")...).Persona; p == nil || p.Name != "other" || p.Prompt != "You are by path." {
		t.Fatalf("path persona = %+v", p)
	}
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "again", Persona: p}, time.Now().Add(-time.Hour))
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if got := ask(append(base, "--retry", rec.ID)...).Persona; got == nil || got.Name != "tester" {
		t.Fatalf("retry persona = %+v, want tester", got)
	}
	code, _, stderr := captureOutput(t, func() int { return cmdAsk(append(base, "--agent", "claude:nope", "go")) })
	if code != 2 || stderr == "" {
		t.Fatalf("unknown persona: exit %d stderr %q, want 2 with a message", code, stderr)
	}
}
