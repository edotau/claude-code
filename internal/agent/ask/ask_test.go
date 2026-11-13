package ask

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"github.com/edotau/claude-code/internal/worktree"
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
		{name: "fresh --keep-env is recorded", flags: []string{"--keep-env", "LANG,TZ"},
			wantTimeout: 10 * time.Minute, wantKeepEnv: []string{"LANG", "TZ"}},
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
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
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(args) })
			if code != 1 {
				t.Fatalf("exit = %d, want 1 for unavailable agent; stderr=%s", code, stderr)
			}
			records := agent.RecentRecords(2)
			if len(records) != wantRecords {
				t.Fatalf("records = %d, want %d; stderr=%s", len(records), wantRecords, stderr)
			}
			if strings.Contains(stderr, "ask: dispatch") { // --format json carries dispatch_id in the envelope
				t.Errorf("stderr = %q, want no dispatch line under --format json", stderr)
			}
			assertAskRetryRequest(t, dir, tt, records[0].Request)
		})
	}
}

// --background and --job are mutually exclusive: Cmd must fail before doing any work.
func TestCmdAskBackgroundAndJobMutuallyExclusive(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--job", rec.ID}) })
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
	skipWithoutSh(t)
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

// runAskJobIntoLog mirrors agent.Spawn: the job child's own stdout and stderr are the log file, which it returns.
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

// A --job run's stdout+stderr are the log, so the vendor child's stderr is teed once and the answer not doubled.
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

// hostFixture installs a fake ssh on PATH and a fake remote bin printing banner then NDJSON lines; returns its path.
func hostFixture(t *testing.T, banner string, lines []string, exit int) string {
	t.Helper()
	skipWithoutSh(t)
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

func ndjson(t *testing.T, evs ...wire.Event) []string {
	t.Helper()
	var out []string
	for i, ev := range evs {
		ev.Schema, ev.Seq, ev.DispatchID = wire.EventSchema, int64(i+1), "REMOTE-ID"
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

func resultEvent(env wire.Envelope) wire.Event {
	env.Schema = wire.EnvelopeSchema
	return wire.Event{Type: wire.EventResult, Host: "remotebox", Envelope: &env}
}

func assertRelayedStream(t *testing.T, evs []wire.Event) string {
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

func assertRelayedEnvelope(t *testing.T, got *wire.Envelope, local string) {
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

// ask --host over a fake ssh: remote start dropped, events keep the host stamp, envelope carried, banner logged.
func TestAskHostRelaysRemoteStream(t *testing.T) {
	env := wire.Envelope{Status: wire.StatusOK, Agent: "claude", Answer: "hi there", SessionID: "sess-9",
		Usage:      &wire.Usage{InputTokens: 3, OutputTokens: 4, CostUSD: 0.0123},
		Structured: json.RawMessage(`{"k":1}`), Emulated: []string{"--output-schema"}, Dropped: []string{"--effort"}}
	lines := ndjson(t,
		wire.Event{Type: wire.EventStart, Host: "remotebox"},
		wire.Event{Type: wire.EventText, Host: "remotebox", Text: "hi there"},
		wire.Event{Type: wire.EventToolUse, Host: "remotebox", Tool: &wire.ToolRef{ID: "t1", Name: "Read"}},
		resultEvent(env))
	bin := hostFixture(t, "Welcome to fakehost banner", lines, 0)
	att := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(att, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out := captureAskStdout(t, "--host", "fakehost", "--host-bin", bin, "--format", "stream-json",
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

func lastEnvelope(t *testing.T, out string) *wire.Envelope {
	t.Helper()
	evs := decodeEvents(t, out)
	e := evs[len(evs)-1]
	if e.Type != wire.EventResult || e.Envelope == nil {
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
			return ndjson(t, resultEvent(wire.Envelope{Status: wire.StatusError, Agent: "claude",
				Error: &wire.EnvelopeError{Kind: "schema", Message: "answer fails the schema"}}))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var lines []string
			if c.lines != nil {
				lines = c.lines(t)
			}
			bin := hostFixture(t, "", lines, c.exit)
			code, out := captureAskStdout(t, "--host", "fakehost", "--host-bin", bin, "--format", "stream-json", "go")
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
	lines := ndjson(t, resultEvent(wire.Envelope{Status: wire.StatusOK, Agent: "claude", Answer: "ok"}))
	bin := hostFixture(t, "", lines, 0)
	calls := recordingRemote(t, bin)
	if code, out := captureAskStdout(t, "--host", "fakehost", "--host-bin", bin, "--fallback", "agents",
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
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--env", arg, "hi"}) })
		if code == 0 || strings.Contains(stdout+stderr, "supersecret") {
			t.Errorf("--env %s: code=%d, value leaked or accepted:\nstdout=%q\nstderr=%q", arg, code, stdout, stderr)
		}
	}
}

// ask --retry of a --host dispatch replays it on the same host through the same remote binary, never locally.
func TestAskRetryOfHostDispatchRunsRemote(t *testing.T) {
	lines := ndjson(t, resultEvent(wire.Envelope{Status: wire.StatusOK, Agent: "claude", Answer: "ok"}))
	bin := hostFixture(t, "", lines, 0)
	calls := recordingRemote(t, bin)
	code, out := captureAskStdout(t, "--host", "fakehost", "--host-bin", bin, "--format", "json", "go")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var env wire.Envelope
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
	lines := ndjson(t, resultEvent(wire.Envelope{Status: wire.StatusOK, Agent: "claude", Answer: "ok"}))
	bin := hostFixture(t, "", lines, 0)
	calls := recordingRemote(t, bin)
	code, out := captureAskStdout(t, "--host", "fakehost", "--host-bin", bin, "--fallback", "none", "--format", "json", "go")
	var env wire.Envelope
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
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(args) })
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
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return Cmd(args) })
	return code, stdout
}

func decodeEvents(t *testing.T, s string) []wire.Event {
	t.Helper()
	var evs []wire.Event
	for l := range strings.SplitSeq(strings.TrimSpace(s), "\n") {
		var ev wire.Event
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("line %q is not an event: %v", l, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

// streamTypesAndText returns the comma-joined event types and the concatenated text, checking seq rises.
func streamTypesAndText(t *testing.T, evs []wire.Event) (string, string) {
	t.Helper()
	var types []string
	var text strings.Builder
	for i, ev := range evs {
		types = append(types, ev.Type)
		if ev.Type == wire.EventText {
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

func assertEventsLogMirrors(t *testing.T, evs []wire.Event, dispatchID, secret string) {
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

// ask --format stream-json over a fake claude: ordered spans, answer equals streamed text, events log mirrors stdout.
func TestAskStreamJSONSpansEventsAndLog(t *testing.T) {
	skipWithoutSh(t)
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
	if len(evs) != 1 || evs[0].Type != wire.EventResult || evs[0].Envelope == nil || evs[0].Envelope.Status == "" {
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
	skipWithoutSh(t)
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

func TestCmdAskPackFlagExitsTwo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--index", "x"}) }); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agent.StubProxyForTest("http://127.0.0.1:9999", "rtr-secret-test") // the batch/envelope tests reach the proxy
	agent.StubRefreshListingForTest()
	agentconfig.SetCatalogWarn(io.Discard)
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// D1: a named --profile leads with the ladder unless --fallback none pins it to one hop.
func TestResolveLeadNamedProfileLeads(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"qa":{"preset":"databricks",`+
		`"host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	cases := []struct {
		args               []string
		wantLead, wantProv string
	}{
		{[]string{"--profile", "qa"}, "qa", ""},
		{[]string{"--profile", "qa", "--fallback", "agents"}, "qa", ""},
		{[]string{"--profile", "qa", "--fallback", "none"}, "", "qa"},
	}
	for _, c := range cases {
		fs, f := askFlagSet()
		if err := fs.Parse(c.args); err != nil {
			t.Fatal(err)
		}
		r := &askRun{fs: fs, f: f, ag: "codex"}
		if code, done := r.resolveLead(); done {
			t.Fatalf("%v: resolveLead exit %d", c.args, code)
		}
		if r.lead != c.wantLead || f.provider != c.wantProv {
			t.Errorf("%v: lead %q profile %q, want lead %q profile %q", c.args, r.lead, f.provider, c.wantLead, c.wantProv)
		}
	}
}

// personaFixture writes agents/tester.md and other.md under a temp config dir with no agent binary on PATH.
func personaFixture(t *testing.T) (other string) {
	t.Helper()
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
	other = filepath.Join(dir, "other.md")
	if err := os.WriteFile(other, []byte("---\ndescription: By path\n---\nYou are by path.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return other
}

// askLoggedRequest runs Cmd against the missing agent binary and returns the request it logged.
func askLoggedRequest(t *testing.T, args ...string) agent.RunOptions {
	t.Helper()
	if code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(args) }); code != 1 {
		t.Fatalf("exit = %d, want 1 for unavailable agent; stderr=%s", code, stderr)
	}
	return agent.RecentRecords(1)[0].Request
}

func assertTesterPersona(t *testing.T, p *agent.Schema) {
	t.Helper()
	if p == nil || p.Name != "tester" || p.Description != "Tests things" || p.Prompt != "You test." || p.Model != "sonnet" ||
		len(p.Tools) != 2 || p.Tools[0] != "Read" {
		t.Fatalf("named persona = %+v", p)
	}
}

// --agent vendor:persona loads agents/<name>.md or a path into the logged request; --retry replays it without the flag.
func TestAskPersonaLoadsAndRetryKeepsIt(t *testing.T) {
	other := personaFixture(t)
	ask := func(args ...string) agent.RunOptions { return askLoggedRequest(t, args...) }
	base := []string{"--fallback", "none", "--format", "json"}
	p := ask(append(base, "--agent", "claude:tester", "go")...).Persona
	assertTesterPersona(t, p)
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(append(base, "--agent", "claude:nope", "go")) })
	if code != 2 || stderr == "" {
		t.Fatalf("unknown persona: exit %d stderr %q, want 2 with a message", code, stderr)
	}
}

// --as is checked against the resolved agent: a claude persona passes, any other agent exits 2.
func TestAskAsChecksResolvedAgent(t *testing.T) {
	personaFixture(t)
	base := []string{"--fallback", "none", "--format", "json", "--as", "sp"}
	run := func(sel string) (int, string) {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(append(append([]string{}, base...), "--agent", sel, "go")) })
		return code, stderr
	}
	if code, stderr := run("claude:tester"); code != 1 || strings.Contains(stderr, "applies to claude only") {
		t.Errorf("claude persona: exit %d stderr %q, want 1 (agent unavailable) with --as accepted", code, stderr)
	}
	for _, sel := range []string{"codex", "codex:tester"} {
		if code, stderr := run(sel); code != 2 || !strings.Contains(stderr, "applies to claude only") {
			t.Errorf("--agent %s --as sp: exit %d stderr %q, want 2 with \"applies to claude only\"", sel, code, stderr)
		}
	}
}

func TestAskRunRetryAgents(t *testing.T) {
	r := &askRun{f: &askFlags{retryAgent: " codex, ,opencode,"}}
	got, err := r.retryAgents()
	if err != nil || strings.Join(got, ",") != "codex,opencode" {
		t.Errorf("retryAgents = %v, %v, want codex,opencode", got, err)
	}
	r.f.retryAgent = "codex,nope-agent"
	if _, err := r.retryAgents(); err == nil || !strings.Contains(err.Error(), "nope-agent") {
		t.Errorf("unknown agent error = %v, want it to name nope-agent", err)
	}
}

func TestBatchFollowFlagErrors(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"follow", []string{"--follow", "hi"}, "--follow needs --batch"},
		{"follow-idle", []string{"--follow-idle", "5s", "hi"}, "--follow-idle needs --batch"},
		{"retry-agent", []string{"--retry-agent", "codex", "hi"}, "--retry-agent needs --batch"},
		{"follow on stdin", []string{"--batch", "-", "--follow"}, "stdin cannot be followed"},
		{"unknown retry agent", []string{"--batch", "-", "--retry-agent", "nope-agent"}, "nope-agent"},
		{"text format", []string{"--batch", "-", "--format", "text"}, "--batch always prints JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cliEnv(t)
			code, out, stderr := runCLI(t, append([]string{"ask"}, c.argv...)...)
			if code != 2 || !strings.Contains(out+stderr, c.want) {
				t.Errorf("exit %d, output %q, want exit 2 naming %q", code, out+stderr, c.want)
			}
		})
	}
}

// runCLI runs `claude-code agents ask <argv>` in-process (argv[0] is "ask"); returns exit code, stdout and stderr.
func runCLI(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var o, e []byte
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); o, _ = io.ReadAll(outR) }() // drained while Cmd runs, so big output cannot block
	go func() { defer wg.Done(); e, _ = io.ReadAll(errR) }()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code := Cmd(argv[1:])
	os.Stdout, os.Stderr = oldOut, oldErr
	outW.Close()
	errW.Close()
	wg.Wait()
	return code, string(o), string(e)
}

// skipWithoutSh skips tests whose fake CLIs (vendors, ssh, gh) are sh scripts, which Windows cannot exec.
func skipWithoutSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLIs are sh scripts")
	}
}

// cliEnv is a hermetic config dir + HOME with the agy login marker, so every fake CLI looks logged in.
func cliEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("OPENCODE_API_KEY", "test-key") // an own-login route for opencode
	for name, profile := range map[string]string{"codex": "chatgpt", "gemini": "antigravity", "opencode": "nvidia"} {
		if err := vault.SetAgentRecord(name, vault.AgentRecord{Profile: profile}); err != nil { // an external agent with no profile is off
			t.Fatal(err)
		}
	}
	marker := gemini.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// cliFakes installs a stand-in child per CLI agent (copilot rides the SDK and is covered in copilot_gate_test.go).
func cliFakes(t *testing.T) {
	t.Helper()
	skipWithoutSh(t)
	pathstest.FakeVendor(t, "claude", "cat > /dev/null; sleep 0.05\n"+
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"sc","model":"fake-claude"}'`+"\n"+
		`printf '%s\n' '{"type":"assistant","message":{"model":"fake-claude","content":[{"type":"text","text":"hello"}]}}'`+"\n"+
		`printf '%s\n' '{"type":"result","result":"hello","usage":{"input_tokens":3,"output_tokens":2}}'`)
	pathstest.FakeVendor(t, "codex", `[ "$1" = login ] && { echo "Logged in using ChatGPT"; exit 0; }`+"\n"+"cat > /dev/null; sleep 0.05; echo hello")
	pathstest.FakeVendor(t, "agy", "cat > /dev/null; sleep 0.05\n"+
		`printf '%s\n' '{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"hello"}}'`+"\n"+
		`printf '%s\n' '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"hello","num_turns":1,"usage":{"input_tokens":3,"output_tokens":2}}}'`)
	pathstest.FakeVendor(t, "opencode", "sleep 0.05\n"+
		`printf '%s\n' '{"type":"text","sessionID":"so","part":{"text":"hello"}}'`+"\n"+
		`printf '%s\n' '{"type":"step_finish","sessionID":"so","part":{"tokens":{"Input":3,"Output":2}}}'`)
}

// cliMarker is a fake child that records its invocation (never a `login` probe) by touching a file.
func cliMarker(t *testing.T, vendor, body string) string {
	t.Helper()
	skipWithoutSh(t)
	marker := filepath.Join(t.TempDir(), vendor+".ran")
	pathstest.FakeVendor(t, vendor, `[ "$1" = login ] && { echo "Logged in using ChatGPT"; exit 0; }`+"\n"+
		"touch "+marker+"\n"+body)
	return marker
}

var profileFor = map[string]string{"claude": "", "codex": "chatgpt", "gemini": "", "opencode": ""}

func decodeEnvelope(t *testing.T, out string) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\n%s", err, out)
	}
	return env
}

// Every agent's --format json success emits the one standard envelope.
func TestEnvelopeContractSuccess(t *testing.T) {
	noUsage := map[string]bool{"codex": true}
	for _, name := range []string{"claude", "codex", "gemini", "opencode"} {
		t.Run(name, func(t *testing.T) {
			cliEnv(t)
			cliFakes(t)
			code, out, stderr := runCLI(t, "ask", "--agent", name, "--profile", profileFor[name], "--fallback", "none", "--native", "--format", "json", "say hello")
			if code != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
			}
			env := decodeEnvelope(t, out)
			checkSuccessEnvelopeFields(t, env, name)
			checkSuccessEnvelopeUsage(t, env, name, noUsage[name])
		})
	}
}

func checkSuccessEnvelopeFields(t *testing.T, env map[string]any, name string) {
	t.Helper()
	if env["schema"] != float64(1) {
		t.Errorf("schema = %v, want 1", env["schema"])
	}
	if env["status"] != "ok" {
		t.Errorf("status = %v, want ok", env["status"])
	}
	if env["agent"] != name {
		t.Errorf("agent = %v, want %s", env["agent"], name)
	}
	// opencode on its own login picks its own model and reports none; the key must still be present.
	if m, ok := env["model"].(string); !ok || (m == "" && name != "opencode") {
		t.Errorf("model = %v, want non-empty", env["model"])
	}
	if id, _ := env["dispatch_id"].(string); id == "" {
		t.Errorf("dispatch_id = %v, want non-empty", env["dispatch_id"])
	}
}

func checkSuccessEnvelopeUsage(t *testing.T, env map[string]any, name string, noUsage bool) {
	t.Helper()
	if v, ok := env["fallback"]; !ok || v != nil {
		t.Errorf("fallback = %v (present=%v), want explicit null", v, ok)
	}
	if d, _ := env["duration_ms"].(float64); d <= 0 {
		t.Errorf("duration_ms = %v, want > 0", env["duration_ms"])
	}
	if noUsage {
		if v, ok := env["usage"]; !ok || v != nil {
			t.Errorf("usage = %v (present=%v), want explicit null: %s reports none", v, ok, name)
		}
	} else if _, ok := env["usage"].(map[string]any); !ok {
		t.Errorf("usage = %v, want an object", env["usage"])
	}
}

// A resume whose launch fails returns an error and never runs another agent's engine.
func TestResumeNeverFallsBack(t *testing.T) {
	cliEnv(t)
	codexRan := cliMarker(t, "codex", "echo 'codex: boom' >&2; exit 1")
	others := map[string]string{
		"claude":   cliMarker(t, "claude", "exit 1"),
		"agy":      cliMarker(t, "agy", "exit 1"),
		"opencode": cliMarker(t, "opencode", "exit 1"),
	}
	code, out, stderr := runCLI(t, "ask", "--agent", "codex", "--session", "sess-1", "continue please")
	if code == 0 {
		t.Errorf("exit = 0, want non-zero: the resumed session's launch failed\nstdout: %s", out)
	}
	if _, err := os.Stat(codexRan); err != nil {
		t.Fatalf("the codex agent was never reached (%v); the test proves nothing\nstderr: %s", err, stderr)
	}
	for name, marker := range others {
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("resume fell back and ran %s; a session belongs to one agent\nstderr: %s", name, stderr)
		}
	}
}

// --profile given to resume reaches the agent's target resolution instead of being dropped.
func TestResumeForwardsProfile(t *testing.T) {
	cliEnv(t)
	cliMarker(t, "codex", "echo ok")
	code, out, stderr := runCLI(t, "ask", "--agent", "codex", "--session", "sess-1", "--profile", "no-such-profile", "continue")
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for an unknown --profile\nstdout: %s", out)
	}
	if !strings.Contains(stderr+out, "no-such-profile") {
		t.Errorf("output never names the --profile given to resume:\nstdout: %s\nstderr: %s", out, stderr)
	}
}

// Every usage failure in --format json mode prints one parseable error envelope and keeps its exit code.
func TestJSONModeFailuresPrintEnvelope(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		code int
	}{
		{"conflicting tools flags", []string{"ask", "--format", "json", "--tools", "--no-tools", "hi"}, 2},
		{"invalid fallback", []string{"ask", "--format", "json", "--fallback", "bogus", "hi"}, 2},
		{"missing prompt", []string{"ask", "--format", "json"}, 2},
		{"retry of unknown id", []string{"ask", "--format", "json", "--retry", "no-such-id"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cliEnv(t)
			cliFakes(t)
			code, out, stderr := runCLI(t, c.argv...)
			if code != c.code {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, c.code, out, stderr)
			}
			env := decodeEnvelope(t, out)
			if env["status"] != "error" {
				t.Errorf("status = %v, want error: %s", env["status"], out)
			}
			e, ok := env["error"].(map[string]any)
			if !ok || e["message"] == "" {
				t.Fatalf("error = %#v, want {kind,message}", env["error"])
			}
			if c.name != "retry of unknown id" && e["kind"] != "usage" {
				t.Errorf("error.kind = %v, want usage", e["kind"])
			}
		})
	}
}

// An unknown --profile is a caller error: all five agents report an error envelope of kind usage, not fatal.
func TestUnknownProfileErrorEnvelopeIsUsageKindForAllAgents(t *testing.T) {
	for _, name := range []string{"claude", "codex", "copilot", "gemini", "opencode"} {
		t.Run(name, func(t *testing.T) {
			cliEnv(t)
			cliFakes(t)
			code, out, stderr := runCLI(t, "ask", "--agent", name, "--profile", "nosuch", "--fallback", "none", "--format", "json", "x")
			if code != 1 {
				t.Errorf("exit = %d, want 1\nstderr: %s", code, stderr)
			}
			env := decodeEnvelope(t, out)
			if env["status"] != "error" {
				t.Errorf("status = %v, want error", env["status"])
			}
			e, _ := env["error"].(map[string]any)
			if e["kind"] != "usage" {
				t.Errorf("error.kind = %v, want usage: %s", e["kind"], out)
			}
			if m, _ := e["message"].(string); m == "" {
				t.Errorf("error.message = %v, want non-empty", e["message"])
			}
		})
	}
}

// --batch validates --provider like a single ask does: an unknown provider exits 2 before any task runs.
func TestBatchRejectsUnknownProvider(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	file := batchFile(t, taskLine(t, map[string]any{"id": "a", "agent": "claude", "prompt": "one", "workdir": t.TempDir(), "tools": true}))
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--provider", "nonexistent")
	if code != 2 || !strings.Contains(stderr, "nonexistent") || strings.TrimSpace(out) != "" {
		t.Fatalf("exit %d, want 2 naming the provider with no envelopes\nstdout: %s\nstderr: %s", code, out, stderr)
	}
}

func TestAskJSONStdoutFailureExitsNonzero(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	args := []string{"--agent", "claude", "--fallback", "none", "--format", "json", "say hello"}
	if code, out := captureAskStdout(t, args...); code != 0 {
		t.Fatalf("healthy stdout: exit %d, output %q", code, out)
	}
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if os.IsNotExist(err) {
		t.Skip("/dev/full is unavailable")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := full.Close(); err != nil {
			t.Error(err)
		}
	})
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		previous := os.Stdout
		os.Stdout = full
		defer func() { os.Stdout = previous }()
		return Cmd(args)
	})
	if code != 1 || !strings.Contains(stderr, "no space left on device") {
		t.Fatalf("full stdout: exit %d, stderr %q; want exit 1 reporting the write error", code, stderr)
	}
	if records := agent.RecentRecords(1); len(records) != 1 || records[0].Status != "ok" {
		t.Fatalf("dispatch must succeed independently of stdout: %+v", records)
	}
}

// Given a background spawn that fails (the test binary is never relaunched), Then the saved record leaves running.
func TestAskSpawnFailureFinishesRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.ClaimRecord("claude", agent.RunOptions{}, time.Now())
	if code := askSpawn(flag.NewFlagSet("ask", flag.ContinueOnError), &agent.Prepared{Record: rec}, false, helptext.Text, "claude"); code != 1 {
		t.Fatalf("askSpawn = %d, want 1", code)
	}
	got, err := agent.LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || got.Error == "" {
		t.Errorf("record = %q/%q, want error with the cause", got.Status, got.Error)
	}
}

// --retry of a databricks:dev run re-arms dev with no --overlay, and refuses a --profile leaving it.
func TestCmdAskRetryReArmsRecordedConfinement(t *testing.T) {
	dump := armChildEnvFixture(t)
	runCLI(t, "ask", "--agent", "codex", "--overlay", `{"provider":"databricks","profile":"dev"}`, "--fallback", "none", "go")
	recs := agent.RecentRecords(1)
	if len(recs) != 1 {
		t.Fatalf("armed run saved %d records, want 1", len(recs))
	}
	unarm := func() { // the armed run exported its confinement into this process and cooled dev on its 402
		if err := os.Remove(router.StatePath()); err != nil {
			t.Fatal(err)
		}
		t.Setenv(providers.EnvSessionProfile, "qa")
		t.Setenv(agentconfig.OverlayVar, "")
	}
	unarm()
	code, _, stderr := runCLI(t, "ask", "--retry", recs[0].ID)
	bins, envs := armDumps(t, dump)
	if code == 0 || !slices.Equal(bins, []string{"codex", "codex"}) {
		t.Fatalf("retry exit %d launches %v, want one more codex dev hop\n%s", code, bins, stderr)
	}
	assertArmDevHop(t, "codex", "linux", envs[1])
	unarm()
	code, _, stderr = runCLI(t, "ask", "--retry", recs[0].ID, "--profile", "chatgpt")
	if bins, _ = armDumps(t, dump); code != 2 || !strings.Contains(stderr, "--profile chatgpt: outside overlay databricks:dev") || len(bins) != 2 {
		t.Fatalf("retry --profile chatgpt: exit %d launches %v stderr %q, want 2 refusing outside databricks:dev", code, bins, stderr)
	}
}

const acmeProvider = `"providers":{"openai":{"acme":{"protocol":"openai","base_url":"https://acme.example/v1","auth":{"type":"none"}}}}`

// profileFixture isolates the vault and seeds provider acme (no opencodeID).
func profileFixture(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	doc := `{` + acmeProvider + `}`
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "providers", doc) }); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
}

func TestClientPrepareHubNativeFallback(t *testing.T) {
	profileFixture(t)
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
	prev := RouteRefresh
	RouteRefresh = func(context.Context, string) bool { refreshes++; return false }
	t.Cleanup(func() { RouteRefresh = prev })
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
	profileFixture(t)
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
	profileFixture(t)
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

// A background start whose job-id line cannot be written reports the write error instead of exiting 0 silently.
func TestReportBackgroundStartReturnsWriteError(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close() // a reader-less pipe fails every write with EPIPE
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig; w.Close() })
	for _, format := range []string{helptext.Text, helptext.JSON} {
		if err := reportBackgroundStart(format, agent.Record{ID: "j1"}); err == nil {
			t.Errorf("%s: write to a closed pipe returned nil", format)
		}
	}
}

func TestMemoryRunOptionsNeverFallBack(t *testing.T) {
	o := memoryRunOptions(context.Background(), "p", "/w")
	if o.Fallback != router.FallbackNone || o.Agent != "claude" || !o.Tools || !o.Gate || o.WorkDir != "/w" {
		t.Fatalf("memoryRunOptions = %+v, want claude with tools and FallbackNone", o)
	}
}

// The memory job's `cd` rule reaches its bank directories (and below) and nothing beside or above them.
func TestMemoryRunOptionsCdCoversOnlyBankDirs(t *testing.T) {
	p := memoryRunOptions(context.Background(), "p", t.TempDir()).Permissions
	store := filepath.Join(paths.ConfigDir(), "memory")
	home := filepath.Join(store, "home")
	for cmd, want := range map[string]agent.Decision{
		"cd " + home: agent.Allow, "cd " + home + "/sub": agent.Allow,
		"cd " + store: agent.Deny, "cd " + store + "/other-bank": agent.Deny, "cd " + home + "-evil": agent.Deny,
		"cd " + home + "/../other-bank": agent.Deny, "cd /": agent.Deny, "cd": agent.Deny,
	} {
		if got := p.Decide("Bash", cmd); got != want {
			t.Errorf("Decide(Bash, %q) = %v, want %v", cmd, got, want)
		}
	}
}
