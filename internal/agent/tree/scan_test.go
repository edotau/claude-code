package tree

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLastActivityLineBoundaries(t *testing.T) {
	const records = `not JSON
{"type":"assistant","timestamp":"2026-10-18T03:00:00Z","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/repo/old.go"}},{"type":"text","text":"old"}]}}
{"type":"user","message":{"content":[{"type":"text","text":"ignore"}]}}

{"type":"assistant","timestamp":"2026-10-18T03:01:00Z","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/new.go"}},{"type":"text","text":"new"}]}}`
	for _, ending := range []string{"", "\n", "\r\n"} {
		t.Run("ending="+ending, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.jsonl")
			if err := os.WriteFile(path, []byte(records+ending), 0o600); err != nil {
				t.Fatal(err)
			}
			act := lastActivity(path, "/repo")
			want := Activity{Tool: "Edit", Arg: "new.go", Text: "new", At: time.Date(2026, 10, 18, 3, 1, 0, 0, time.UTC)}
			if act != want {
				t.Fatalf("activity = %+v, want %+v", act, want)
			}
		})
	}
}

func TestUserTurnTextConcatenatesTextBlocks(t *testing.T) {
	raw := []byte(`{"content":[{"type":"text","text":"first "},{"type":"tool_use","name":"X"},{"type":"text","text":"second"}]}`)
	if got := userTurnText(raw); got != "first second" {
		t.Errorf("userTurnText = %q, want %q", got, "first second")
	}
}

// (4) lastActivity's Arg strips both the session cwd and $HOME, for each tool shape the plan names:
// Read/file_path, Edit/file_path, Bash/command (truncated), Grep/pattern.
func TestLastActivityStripsHomeAndCwdPerTool(t *testing.T) {
	// Fixed, short strings (not t.TempDir()) so Bash's command truncation (60 runes) never clips the
	// path before stripPaths gets to it — toolArg truncates, then lastActivity strips what remains.
	const home = "/Users/fakehome"
	const cwd = "/Users/fake/demo-repo"
	t.Setenv("HOME", home)

	cases := []struct {
		name  string
		tool  string
		input string // raw JSON "input" object
		want  string
	}{
		{"Read", "Read", `{"file_path":"` + cwd + `/internal/agenttree/scan.go"}`, "internal/agenttree/scan.go"},
		// $HOME (not cwd) appears here, so stripPaths should collapse it to "~".
		{"Edit", "Edit", `{"file_path":"` + home + `/.claude/CLAUDE.md"}`, "~/.claude/CLAUDE.md"},
		{"Bash", "Bash", `{"command":"go test ` + cwd + `/internal/agenttree/..."}`, "go test internal/agenttree/..."},
		// No "path" field: toolArg prefers file_path/path over pattern, so this exercises the Pattern branch.
		{"Grep", "Grep", `{"pattern":"readJournal mentioned in ` + cwd + `"}`, "readJournal mentioned in "},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent-x.jsonl")
			line := `{"parentUuid":null,"isSidechain":true,"agentId":"x","type":"assistant","timestamp":"2026-10-18T03:00:00Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"` + c.tool + `","input":` + c.input + `}]}}`
			if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			act := lastActivity(path, cwd)
			if act.Tool != c.tool {
				t.Errorf("Tool = %q, want %q", act.Tool, c.tool)
			}
			if act.Arg != c.want {
				t.Errorf("Arg = %q, want %q", act.Arg, c.want)
			}
			if act.At.IsZero() {
				t.Error("At should be the record's own timestamp, not zero")
			}
		})
	}
}

// withDispatchFixture points internal/agent's dispatch log at testdata/dispatch for the duration of
// the test, via CLAUDE_CONFIG_DIR (internal/paths.StateDir honors it).
func withDispatchFixture(t *testing.T) {
	t.Helper()
	cfgDir := t.TempDir()
	dst := filepath.Join(cfgDir, "state", "harness", "dispatch")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("testdata", "dispatch")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
}

// A dispatch id printed only as the envelope "dispatch_id" (no `ask: dispatch` line) still yields its Dispatch.
func TestBuildDispatchesEnvelopeID(t *testing.T) {
	withDispatchFixture(t)
	path := filepath.Join(t.TempDir(), "agent.jsonl")
	line := `{"type":"user","message":{"content":[{"type":"tool_result","content":"{\"dispatch_id\":\"20261017T231131.948-codex\"}"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	got := buildDispatches(path, "", time.Now(), Activity{})
	if len(got) != 1 || got[0].ID != "20261017T231131.948-codex" {
		t.Fatalf("buildDispatches = %+v, want the envelope dispatch", got)
	}
}
