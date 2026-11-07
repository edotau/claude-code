package ask

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
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

type askRetryCase struct {
	name        string
	retry       bool
	flags       []string
	attachments []string
	wantTimeout time.Duration
	wantAttach  []string
	wantKeepEnv []string
	recPerms    bool
	wantAllow   []string
	wantDeny    []string
}

func askRetryCases() []askRetryCase {
	return []askRetryCase{
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
		{name: "retry keeps recorded permissions", retry: true, recPerms: true,
			wantTimeout: 5 * time.Minute, wantAttach: []string{"a.txt"}, wantAllow: []string{"Bash(ls:*)"}, wantDeny: []string{"Bash(rm:*)"}},
		{name: "retry --allow replaces recorded allow only", retry: true, recPerms: true, flags: []string{"--allow", "Read"},
			wantTimeout: 5 * time.Minute, wantAttach: []string{"a.txt"}, wantAllow: []string{"Read"}, wantDeny: []string{"Bash(rm:*)"}},
	}
}

// askRetryArgs builds the Cmd argv for a case, saving the recorded run first when it is a retry.
func askRetryArgs(t *testing.T, dir string, tt askRetryCase) ([]string, int) {
	t.Helper()
	args := []string{"--agent", "claude", "--fallback", "none", "--format", "json"}
	wantRecords := 1
	if tt.retry {
		opts := agent.RunOptions{
			Prompt: "say ok", Tools: true, Timeout: 5 * time.Minute,
			Attach: []string{filepath.Join(dir, "a.txt")},
		}
		if tt.recPerms {
			p, err := agent.ParsePermissions("", []string{"Bash(ls:*)"}, nil, []string{"Bash(rm:*)"})
			if err != nil {
				t.Fatal(err)
			}
			opts.Permissions = p
		}
		rec := agent.NewRecord("claude", opts, time.Now().Add(-time.Hour))
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
	return args, wantRecords
}

func assertAskRetryRequest(t *testing.T, dir string, tt askRetryCase, req agent.RunOptions) {
	t.Helper()
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
	if got := agent.RuleStrings(req.Permissions.Allow); !slices.Equal(got, tt.wantAllow) {
		t.Errorf("allow = %q, want %q", got, tt.wantAllow)
	}
	if got := agent.RuleStrings(req.Permissions.Deny); !slices.Equal(got, tt.wantDeny) {
		t.Errorf("deny = %q, want %q", got, tt.wantDeny)
	}
}

// A malformed permission rule exits 2 before any dispatch is recorded or launched.
func TestCmdAskBadPermissionRuleNeverLaunches(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	code, _, stderr := captureOutput(t, func() int {
		return Cmd([]string{"--agent", "claude", "--fallback", "none", "--deny", "Bash(", "say ok"})
	})
	if code != 2 || !strings.Contains(stderr, `permission rule "Bash("`) {
		t.Fatalf("exit = %d, stderr = %q; want 2 naming the rule", code, stderr)
	}
	if n := len(agent.RecentRecords(2)); n != 0 {
		t.Fatalf("%d dispatch records written; the bad rule must stop before launch", n)
	}
}

func TestCmdAskRetryFlags(t *testing.T) {
	for _, tt := range askRetryCases() {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			t.Setenv("HOME", dir)
			t.Setenv("PATH", dir)
			for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
				writeFixtureFile(t, filepath.Join(dir, name), name)
			}
			args, wantRecords := askRetryArgs(t, dir, tt)
			code, _, stderr := captureOutput(t, func() int { return Cmd(args) })
			if code != 1 {
				t.Fatalf("exit = %d, want 1 for unavailable agent; stderr=%s", code, stderr)
			}
			records := agent.RecentRecords(2)
			if len(records) != wantRecords {
				t.Fatalf("records = %d, want %d; stderr=%s", len(records), wantRecords, stderr)
			}
			wantDispatch := "ask: dispatch " + records[0].ID +
				" (agents result <id> reprints it; agents ask --retry <id> replays it)"
			if lineCount(stderr, wantDispatch) != 1 {
				t.Errorf("stderr = %q, want one line %q", stderr, wantDispatch)
			}
			assertAskRetryRequest(t, dir, tt, records[0].Request)
		})
	}
}

// --background and --job are mutually exclusive: Cmd must fail before doing any work.
func TestCmdAskBackgroundAndJobMutuallyExclusive(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return Cmd([]string{"--background", "--job", "some-id", "say ok"})
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
	code, _, stderr := captureOutput(t, func() int { return Cmd([]string{"--job", rec.ID}) })
	if code != 2 || !strings.Contains(stderr, "not a fresh background job") {
		t.Fatalf("exit %d stderr %q, want 2 and the not-a-fresh-job message", code, stderr)
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

// agyJobFixture logs agy in under a temp HOME and fakes the vendor binary with stderr noise plus a streamed answer.
func agyJobFixture(t *testing.T) {
	t.Helper()
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
}

// runAskJobIntoLog mirrors job.Spawn: the job child's own stdout and stderr are the log file, which it returns.
func runAskJobIntoLog(t *testing.T) string {
	t.Helper()
	rec := agent.NewRecord("gemini", agent.RunOptions{Prompt: "p", WorkDir: t.TempDir()}, time.Now())
	rec.Log = rec.LogPath()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	lf, err := os.OpenFile(rec.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = lf, lf
	code := Cmd([]string{"--job", rec.ID, "--native", "--fallback", "none"})
	os.Stdout, os.Stderr = oldOut, oldErr
	lf.Close()
	b, err := os.ReadFile(rec.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, b)
	}
	return string(b)
}

// A --job run's stdout+stderr already are the log (Spawn), so the vendor child's stderr must be teed in
// exactly once and the streamed answer must not be doubled by a second stdout tee.
func TestCmdAskJobLogEachLineOnce(t *testing.T) {
	agyJobFixture(t)
	got := runAskJobIntoLog(t)
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
	line := ParsedLine{N: 1, Task: TaskLine{Prompt: "p", Profile: "acme"}}
	bt, err := BuildTask(line, Opts{Agent: "codex", Fallback: router.FallbackAll}, agent.RunOptions{}, false, t.TempDir(), map[string]bool{}, NewClient)
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
	res, err := NewClient().Exec(context.Background(), p, nil, agent.RunRecordedOpts{})
	if err == nil || len(res.Hops) != 1 {
		t.Fatalf("Exec: %d hops, err %v; want one unavailable dispatch", len(res.Hops), err)
	}
	bad := ParsedLine{N: 2, Task: TaskLine{Prompt: "p", Profile: "no-such-profile"}}
	bt, err = BuildTask(bad, Opts{Agent: "codex", Fallback: router.FallbackAll}, agent.RunOptions{}, false, t.TempDir(), map[string]bool{}, NewClient)
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

func assertRelayedStream(t *testing.T, evs []agent.Event) string {
	t.Helper()
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
	return local
}

func assertRelayedEnvelope(t *testing.T, got *agent.Envelope, local string) {
	t.Helper()
	if got == nil || got.Usage == nil || got.Usage.CostUSD != 0.0123 || string(got.Structured) != `{"k":1}` {
		t.Fatalf("result envelope lost remote fields: %+v", got)
	}
	if got.SessionID != "sess-9" || got.Answer != "hi there" || got.DispatchID != local {
		t.Fatalf("result envelope lost remote fields: %+v", got)
	}
	if !slices.Contains(got.Emulated, "--output-schema") {
		t.Errorf("emulated = %v", got.Emulated)
	}
	assertDropped(t, got.Dropped, "--attach", "--keep-env", "--effort")
}

func assertDropped(t *testing.T, dropped []string, want ...string) {
	t.Helper()
	for _, d := range want {
		if !slices.Contains(dropped, d) {
			t.Errorf("dropped %v lacks %s", dropped, d)
		}
	}
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
	local := assertRelayedStream(t, evs)
	assertRelayedEnvelope(t, evs[3].Envelope, local)
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
		code, stdout, stderr := captureOutput(t, func() int { return Run([]string{"--env", arg, "hi"}, false) })
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
	code, stdout, stderr := captureOutput(t, func() int { return Cmd(args) })
	var env struct{ Agent string }
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || code != 2 {
		t.Fatalf("exit %d, stdout %q, err %v, stderr %q; want a usage envelope", code, stdout, err, stderr)
	}
	if env.Agent != "codex" {
		t.Fatalf("envelope agent = %q, want the resolved agent codex", env.Agent)
	}
}

// captureAskStdout runs Cmd and returns its exit code and stdout.
func captureAskStdout(t *testing.T, args ...string) (int, string) {
	t.Helper()
	code, stdout, _ := captureOutput(t, func() int { return Cmd(args) })
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

// streamTypesAndText returns the comma-joined event types and the concatenated text, checking seq rises.
func streamTypesAndText(t *testing.T, evs []agent.Event) (string, string) {
	t.Helper()
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
	return strings.Join(types, ","), text.String()
}

func assertEventsLogMirrors(t *testing.T, evs []agent.Event, dispatchID, secret string) {
	t.Helper()
	logb, err := os.ReadFile(filepath.Join(paths.DispatchDir(), dispatchID+".events.jsonl"))
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
	types, text := streamTypesAndText(t, evs)
	if want := "start,text,tool_use,text,result"; types != want {
		t.Fatalf("types = %v, want %s", types, want)
	}
	if strings.Contains(text, "SUBAGENT") {
		t.Errorf("a parent_tool_use_id turn leaked into text events: %q", text)
	}
	res := evs[len(evs)-1]
	if res.Envelope == nil || res.Envelope.Answer != text {
		t.Fatalf("result envelope answer = %+v, concatenated text = %q", res.Envelope, text)
	}
	if res.DispatchID == "" || res.Envelope.DispatchID != res.DispatchID {
		t.Errorf("dispatch ids: result %q, envelope %+v", res.DispatchID, res.Envelope)
	}
	assertEventsLogMirrors(t, evs, res.DispatchID, secret)
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

type askOutcome struct {
	code int
	out  string
}

// waitFakeVendorReady polls for the ready marker, failing if the run ends first or the vendor never starts.
func waitFakeVendorReady(t *testing.T, ready string, done <-chan askOutcome) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return
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
	done := make(chan askOutcome, 1)
	go func() {
		code, out := captureAskStdout(t, "--format", "json", "--fallback", "none", "--stdin-framed")
		done <- askOutcome{code, out}
	}()
	waitFakeVendorReady(t, ready, done)
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

// Given pack flags that cannot work together or alone, askPack refuses each; valid ones build the spec the request carries.
func TestAskPackFlagCombos(t *testing.T) {
	refused := []struct {
		name string
		args []string
		want string
	}{
		{"index alone", []string{"--index"}, "--index needs"},
		{"max-files alone", []string{"--max-files", "3"}, "--max-files needs"},
		{"max-file-bytes alone", []string{"--max-file-bytes", "9"}, "--max-file-bytes needs"},
		{"negative max-files", []string{"--dirs", ".", "--max-files", "-1"}, "negative"},
		{"batch", []string{"--dirs", ".", "--batch", "-"}, "--batch"},
		{"live", []string{"--files", "*.go", "--live"}, "--live"},
		{"host", []string{"--diff", "--host", "box"}, "--host"},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			fs, f := askFlagSet()
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			if _, err := askPack(fs, f); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("askPack(%v) = %v, want an error containing %q", tt.args, err, tt.want)
			}
		})
	}
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--dirs", "a,b", "--index", "--max-files", "0", "--diff"}); err != nil {
		t.Fatal(err)
	}
	f.pack, _ = askPack(fs, f)
	req := (&askRun{f: f}).request("task")
	if p := req.Pack; p == nil || len(p.Dirs) != 2 || !p.Diff || !p.Index || !p.IndexOnly {
		t.Fatalf("request.Pack = %+v, want dirs a,b + diff + index + index-only", p)
	}
}

func TestCmdAskPackFlagExitsTwo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	if code, _, _ := captureOutput(t, func() int { return Cmd([]string{"--index", "x"}) }); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
