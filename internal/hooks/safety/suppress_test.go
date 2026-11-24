package safety

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// suppressCase is one edit call, its verdict and a substring the message must carry.
type suppressCase struct {
	name  string
	tool  string
	input map[string]any
	agent string
	want  core.Decision
	text  string
}

// Added suppression lines ask in the main session and block in an agent; untouched ones and prose pass.
func TestSuppressVerdictTable(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.py")
	if err := os.WriteFile(old, []byte("a = 1  # noqa: E501\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := append(suppressEditCases(dir, old), suppressMarkerCases(dir)...)
	for _, c := range cases {
		doc := map[string]any{"tool_name": c.tool, "cwd": dir, "tool_input": c.input}
		if c.agent != "" {
			doc["agent_id"] = c.agent
		}
		payload, _ := json.Marshal(doc)
		got, msg := safetyScanDecision(t, bytes.NewReader(payload))
		if got != c.want || !strings.Contains(msg, c.text) {
			t.Errorf("%s: got %v %q, want %v containing %q", c.name, got, msg, c.want, c.text)
		}
	}
}

// suppressEditCases are the Edit, Write, notebook and prose rows.
func suppressEditCases(dir, old string) []suppressCase {
	edit := func(path, oldS, newS string) map[string]any {
		return map[string]any{"file_path": path, "old_string": oldS, "new_string": newS}
	}
	return []suppressCase{
		{"edit adds noqa, main asks", "Edit", edit(old, "x = 1", "x = 1  # noqa: E501 # long url"), "", core.Ask, "long url"},
		{"edit adds noqa, agent blocks", "Edit", edit(old, "x = 1", "x = 1  # noqa: E501"), "a1", core.Block, "UNFIXED"},
		{"main without reason", "Edit", edit(old, "x", "y: int  # type: ignore[attr-defined]"), "", core.Ask, "no reason given"},
		{"kept existing marker passes", "Edit", edit(old, "a = 1  # noqa: E501\nb", "a = 1  # noqa: E501\nc"), "a1", core.Allow, ""},
		{"write nolint blocks", "Write", map[string]any{"file_path": filepath.Join(dir, "a.go"),
			"content": "f() //nolint:errcheck\n"}, "a1", core.Block, "a.go:1"},
		{"write over file keeps marker", "Write", map[string]any{"file_path": old, "content": "a = 1  # noqa: E501\nb = 2\n"},
			"a1", core.Allow, ""},
		{"marker in a string passes", "Write", map[string]any{"file_path": filepath.Join(dir, "s.py"),
			"content": "m = \"# noqa\"\nk = '# noqa'\nn = 1  // nolinting\n"}, "a1", core.Allow, ""},
		{"marker after a quote char blocks", "Write", map[string]any{"file_path": filepath.Join(dir, "q.py"),
			"content": "x = '\"'  # noqa\n"}, "a1", core.Block, "q.py:1"},
		{"notebook_path blocks", "NotebookEdit", map[string]any{"notebook_path": filepath.Join(dir, "n.ipynb"),
			"new_source": "x = 1  # type: ignore"}, "a1", core.Block, "n.ipynb:1"},
		{"markdown prose passes", "Write", map[string]any{"file_path": filepath.Join(dir, "n.md"),
			"content": "use # noqa sparingly\n"}, "a1", core.Allow, ""},
	}
}

// suppressMarkerCases are one row per added marker, and an .ipynb Write whose code cell carries one.
func suppressMarkerCases(dir string) []suppressCase {
	write := func(name, content string) map[string]any {
		return map[string]any{"file_path": filepath.Join(dir, name), "content": content}
	}
	return []suppressCase{
		{"nosec blocks", "Write", write("sec.go", "x := f() // #nosec G104\n"), "a1", core.Block, "sec.go:1"},
		{"bandit nosec blocks", "Write", write("sec.py", "import os  # nosec\n"), "a1", core.Block, "sec.py:1"},
		{"nosemgrep blocks", "Write", write("sg.go", "f() // nosemgrep: rule-id\n"), "a1", core.Block, "sg.go:1"},
		{"noinspection blocks", "Write", write("i.js", "//noinspection JSUnresolvedVariable\nx()\n"), "a1", core.Block, "i.js:1"},
		{"SuppressWarnings annotation blocks", "Write", write("J.java", "@SuppressWarnings(\"unchecked\")\nvoid f() {}\n"),
			"a1", core.Block, "J.java:1"},
		{"pragma diagnostic ignored blocks", "Write", write("p.c", "#pragma GCC diagnostic ignored \"-Wunused\"\n"),
			"a1", core.Block, "p.c:1"},
		{"pragma warning disable blocks", "Write", write("m.cpp", "#pragma warning(disable)\n"), "a1", core.Block, "m.cpp:1"},
		{"pyre-ignore blocks", "Write", write("t.py", "x = f()  # pyre-ignore[16]\n"), "a1", core.Block, "t.py:1"},
		{"pyre-fixme blocks", "Write", write("t2.py", "x = f()  # pyre-fixme\n"), "a1", core.Block, "t2.py:1"},
		{"ipynb write code cell blocks", "Write", write("n2.ipynb",
			`{"cells":[{"cell_type":"code","source":["x = 1  # type: ignore"]}]}`), "a1", core.Block, "n2.ipynb:1"},
	}
}
