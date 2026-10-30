package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

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
	if !contains(got.Emulated, "--output-schema") {
		t.Errorf("emulated = %v", got.Emulated)
	}
	for _, d := range []string{"--attach", "--keep-env", "--effort"} {
		if !contains(got.Dropped, d) {
			t.Errorf("dropped %v lacks %s", got.Dropped, d)
		}
	}
	logb, err := os.ReadFile(filepath.Join(agent.DispatchDir(), local+".log"))
	if err != nil || !strings.Contains(string(logb), "Welcome to fakehost banner") {
		t.Fatalf("banner not in dispatch log (err %v):\n%s", err, logb)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
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
