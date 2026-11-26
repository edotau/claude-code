package codex

import (
	"slices"
	"strconv"
	"time"

	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/paths/pathstest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/agent/wire"
)

// resetCodexJSON clears the process memo now and again when the test ends.
func resetCodexJSON(t *testing.T) {
	t.Helper()
	codexJSON.Drop()
	t.Cleanup(codexJSON.Drop)
}

func TestCodexHasJSONCachesOnlyDefinitiveAnswers(t *testing.T) {
	origTimeout := agent.ProbeTimeout
	agent.ProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { agent.ProbeTimeout = origTimeout })
	resetCodexJSON(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pathstest.FakeVendor(t, "codex", "exec sleep 5") // exec: the kill reaches sleep, not an orphan holding the pipe
	if codexHasJSON() {
		t.Fatal("timed-out probe reported --json")
	}
	agent.ProbeTimeout = origTimeout // later probes must answer, not race the short timeout under load
	pathstest.FakeVendor(t, "codex", "echo '  --json  print events'")
	if !codexHasJSON() {
		t.Fatal("slow first probe stuck codexHasJSON at false; want a re-probe")
	}
	pathstest.FakeVendor(t, "codex", "exit 0")
	if !codexHasJSON() {
		t.Fatal("definitive answer was not cached")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // a fresh disk cache and a fresh process memo
	resetCodexJSON(t)
	pathstest.FakeVendor(t, "codex", "echo '  --help only'")
	if codexHasJSON() {
		t.Fatal("help without --json reported true")
	}
	resetCodexJSON(t)
	pathstest.FakeVendor(t, "codex", "echo '  --json  print events'")
	if !codexHasJSON() {
		t.Fatal("a false answer reached the disk cache; an upgrade would never be re-probed")
	}
}

// A persisted false is ignored and replaced; a no re-probe drops it.
func TestCodexHasJSONDropsStaleFalseRow(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	resetCodexJSON(t)
	bin := pathstest.FakeVendor(t, "codex", "echo '  --json  print events'")
	tools.ProbedSaltedLine(bin, "exec-json", func(string) string { return "false" }) // persisted by an older build
	if !codexHasJSON() {
		t.Fatal("a persisted false was honored; want it ignored and re-probed")
	}
	if got := tools.ProbedSaltedLine(bin, "exec-json", func(string) string { return "" }); got != "true" {
		t.Fatalf("cached row after a yes re-probe = %q, want true", got)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	resetCodexJSON(t)
	bin = pathstest.FakeVendor(t, "codex", "echo '  --help only'")
	tools.ProbedSaltedLine(bin, "exec-json", func(string) string { return "false" })
	if codexHasJSON() {
		t.Fatal("help without --json reported true")
	}
	if got := tools.ProbedSaltedLine(bin, "exec-json", func(string) string { return "" }); got != "" {
		t.Fatalf("stale false row survived a no re-probe: %q", got)
	}
}

func TestCodexPermissionDenialNamesRealControl(t *testing.T) {
	cmd := "/opt/homebrew/bin/bash -lc 'make lint'"
	cases := []struct {
		name, out, sandbox string
		tools              bool
		layer, remedy      string
		replay             bool
	}{
		{name: "read-only sandbox write", out: "touch: x: Operation not permitted", sandbox: "read-only", layer: "os-sandbox", remedy: "--tools"},
		{name: "workspace write outside", out: "open /etc/x: read-only file system", sandbox: "workspace-write", tools: true,
			layer: "os-sandbox", remedy: "sandboxMode"},
		{name: "sealed network", out: "Network access was denied by the Codex sandbox network proxy.", sandbox: "workspace-write", tools: true,
			layer: "os-sandbox", remedy: "codexNetwork"},
		{name: "foreign rule file", out: "command was blocked by policy", tools: true, layer: "codex-rules", remedy: "rule file"},
		{name: "gate safety block", out: "Command blocked by PreToolUse hook: safety", tools: true, layer: "hook", remedy: "no override"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, ok := codexDenial(cmd, c.out, c.tools, c.sandbox)
			if !ok || d.Layer != c.layer || !strings.Contains(d.Remedy, c.remedy) || d.Call.Arg != "make lint" {
				t.Fatalf("denial = %+v ok %v, want layer %q remedy containing %q for `make lint`", d, ok, c.layer, c.remedy)
			}
			if c.replay != (d.Replay != "") || (!c.replay && strings.Contains(d.Remedy, "--permissions")) {
				t.Fatalf("denial %+v offers an allow where %v is the only repair", d, c.layer)
			}
		})
	}
	if _, ok := codexDenial(cmd, "ls: /x: No such file or directory", true, "workspace-write"); ok {
		t.Fatal("an ordinary failure was reported as a refusal")
	}
}

func TestCodexPermissionDenialSinkWarns(t *testing.T) {
	use := wire.ToolEvent("shell", "item_1")
	use.Text = "/bin/bash -lc 'touch x'"
	result := func(status, out string) wire.Event {
		return wire.Event{Type: wire.EventToolResult, Tool: &wire.ToolRef{Name: "shell", ID: "item_1"}, Status: status, Text: out}
	}
	text := func(out string) wire.Event { return wire.Event{Type: wire.EventText, Text: out} }
	cases := []struct {
		name   string
		events []wire.Event
		call   string // the warning's named call
	}{
		{"failed result", []wire.Event{use, result("completed (exit 0)", "Operation not permitted"), result("failed (exit 1)", "touch: x: Operation not permitted")}, "Bash(touch x)"},
		{"answer only, once", []wire.Event{text("all good"), text("touch failed: Operation not permitted"), text("again: Operation not permitted")}, "a command"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, warnings := emitThroughDenialSink(t, c.events)
			if len(got) != len(c.events)+1 || len(warnings) != 1 || !strings.Contains(warnings[0], c.call) {
				t.Fatalf("events = %+v, want every event forwarded plus one warning naming %q", got, c.call)
			}
			if !strings.Contains(warnings[0], "--tools") || strings.Contains(warnings[0], "--permissions") {
				t.Fatalf("warning %q must name the sandbox control, not an allow", warnings[0])
			}
		})
	}
}

// emitThroughDenialSink feeds events to a read-only denialSink and returns what it forwarded and its progress warnings.
func emitThroughDenialSink(t *testing.T, events []wire.Event) (got []wire.Event, warnings []string) {
	t.Helper()
	s := &denialSink{next: sinkFunc(func(ev wire.Event) error { got = append(got, ev); return nil }), sandbox: "read-only", commands: map[string]string{}}
	for _, ev := range events {
		if err := s.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	for _, ev := range got {
		if ev.Type == wire.EventProgress {
			warnings = append(warnings, ev.Text)
		}
	}
	return got, warnings
}

type sinkFunc func(wire.Event) error

func (f sinkFunc) Emit(ev wire.Event) error { return f(ev) }

// fixtureDenials replays a stream captured from codex-cli 0.166.0 through a read-only denialSink via cat.
func fixtureDenials(t *testing.T, name string) (wire.Result, error) {
	t.Helper()
	sink := &denialSink{next: wire.Discard, sandbox: "read-only", commands: map[string]string{}}
	cmd, tail := agent.ChildCmd(t.Context(), "/bin/cat", []string{filepath.Join("testdata", name)}, nil, "", "", nil, true)
	return sink.runStream(t.Context(), "codex", cmd, tail, wire.Result{})
}

func TestDenialStreamCollectsRealRefusal(t *testing.T) {
	for _, c := range []struct{ fixture, label string }{
		{"stream-sandbox-refused.jsonl", "go env"},
		{"stream-code-mode-refused.jsonl", "code mode"},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			res, err := fixtureDenials(t, c.fixture)
			if err != nil || res.Denials == nil {
				t.Fatalf("result = %+v, err = %v", res, err)
			}
			if res.Denials.Count != 1 || !slices.Equal(res.Denials.Commands, []string{c.label}) || res.Answer == "" {
				t.Fatalf("denials = %+v, answer = %q", res.Denials, res.Answer)
			}
		})
	}
}

func TestDenialSinkCapsCommandsAndKeepsCount(t *testing.T) {
	var events []wire.Event
	for i, command := range []string{"go test ./...", "python3 -c x", strings.Repeat("界", 81) + " arg", "ps aux"} {
		id := strconv.Itoa(i)
		use := wire.ToolEvent("shell", id)
		use.Text = "/bin/bash -lc '" + command + "'"
		events = append(events, use, wire.Event{Type: wire.EventToolResult, Tool: &wire.ToolRef{Name: "shell", ID: id},
			Status: "failed (exit 1)", Text: "touch: x: Operation not permitted"})
	}
	s := &denialSink{next: wire.Discard, sandbox: "read-only", commands: map[string]string{}}
	for _, ev := range events {
		if err := s.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"go test", "python3 -c", strings.Repeat("界", 80)}
	if s.denials.Count != 4 || !slices.Equal(s.denials.Commands, want) {
		t.Fatalf("denials = %+v, want count 4 and %q", s.denials, want)
	}
}

func TestDenialLabelHidesValues(t *testing.T) {
	for _, c := range []struct{ command, want string }{
		{"/bin/bash -lc 'go test ./...'", "go test"},
		{"python3 -c secret", "python3 -c"},
		{"ps aux", "ps"},
		{"echo hunter2", "echo"},
		{"mysql -phunter2", "mysql"},
		{"/bin/bash -lc 'echo hunter2'", "echo"},
		{"export TOKEN=abc123", "export"},
		{"/bin/bash -lc 'export TOKEN=abc123'", "export"},
		{"env TOKEN=abc123 make", "env"},
		{"FOO=secret cmd", "cmd"},
		{"FOO=secret go test ./...", "go test"},
		{"cat /Users/someone/x", "cat"},
		{"curl https://u:p@host/x", "curl"},
		{"git user@host", "git"},
		{"/usr/bin/env bash -lc 'ps aux'", "ps"},
		{"env FOO=1 /bin/zsh -c 'FOO=secret go vet'", "go vet"},
		{"/Users/someone/bin/tool run", "tool"},
		{"bash -lc 'bash -c \"ls -la\"'", "ls -la"},
		{"rtk proxy go vet ./...", "go vet"},
		{"rtk git status", "git status"},
		{"/bin/bash -lc 'rtk proxy go vet ./...'", "go vet"},
		{"", ""},
		{strings.Repeat("a", 90), strings.Repeat("a", 80)},
	} {
		if got := denialLabel(c.command); got != c.want {
			t.Errorf("denialLabel(%q) = %q, want %q", c.command, got, c.want)
		}
	}
}

func TestCodexShellCommandUnwrapsAnyDashC(t *testing.T) {
	for command, want := range map[string]string{
		"/bin/bash -lc 'make lint'": "make lint",
		"env bash -lc 'make lint'":  "make lint",
		"python3 -c secret":         "secret",
		"make lint":                 "make lint",
	} {
		if got := codexShellCommand(command); got != want {
			t.Errorf("codexShellCommand(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestDenialSinkIgnoresNonCommandRefusals(t *testing.T) {
	clean := &denialSink{next: wire.Discard, sandbox: "read-only", commands: map[string]string{}}
	for _, ev := range []wire.Event{{Type: wire.EventText, Text: "No such file"},
		{Type: wire.EventToolResult, Tool: &wire.ToolRef{ID: "other"}, Status: "failed", Text: "No such file"}} {
		if err := clean.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	if clean.denials != nil {
		t.Fatalf("non-command refusals counted: %+v", clean.denials)
	}
}
