package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths/pathstest"
)

// captureAskStdout runs cmdAsk with os.Stdout redirected to a file and returns its exit code and stdout.
func captureAskStdout(t *testing.T, args ...string) (int, string) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	code := cmdAsk(args)
	os.Stdout = old
	f.Close()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, string(b)
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

	logb, err := os.ReadFile(filepath.Join(agent.DispatchDir(), res.DispatchID+".events.jsonl"))
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
	pathstest.FakeVendor(t, "claude", "cat > /dev/null\nexec sleep 60\n")
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
	time.Sleep(700 * time.Millisecond) // the run is blocked in the fake claude by now
	select {
	case o := <-done:
		t.Fatalf("run ended before stdin closed: %+v", o)
	default:
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
			t.Fatalf("exit = %d, want 1 for unavailable runner; stderr=%s", code, stderr)
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
