package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeContextFixture writes a synthetic transcript that exercises every source, and returns it with the two long texts.
func writeContextFixture(t *testing.T) (path, hook, hand string) {
	t.Helper()
	hook = "<system-reminder>\nUserPromptSubmit hook additional context: <!-- memory-recall " + strings.Repeat("m", 30)
	hand = "Another Claude session sent a message:\n<agent-message from=\"a1\">" + strings.Repeat("h", 100)
	lines := []string{
		`{"type":"user","message":{"content":"0123456789"}}`,
		`{"type":"user","message":{"content":"<agent-message> typed by a human"}}`, // not meta: stays user text
		`{"type":"assistant","message":{"content":[{"type":"text","text":"abcdefghijklmnopqrst"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"cmd":"ls"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + strings.Repeat("r", 50) + `"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"nope","content":[{"type":"text","text":"abc"}]}]}}`,
		`{"type":"attachment","rendered":[{"content":` + quote(hook) + `}],"attachment":{"type":"hook_additional_context","hookName":"UserPromptSubmit"}}`,
		`{"type":"attachment","rendered":[{"content":"` + strings.Repeat("q", 10) + `"}],"attachment":{"type":"queued_command","prompt":"<task-notification>x"}}`,
		`{"type":"attachment","rendered":[{"content":"` + strings.Repeat("o", 7) + `"}],"attachment":{"type":"output_style"}}`,
		`{"type":"attachment","attachment":{"type":"hook_success","hookName":"Stop"}}`,
		`{"type":"user","isMeta":true,"message":{"content":` + quote(hand) + `}}`,
		`{"type":"user","isCompactSummary":true,"message":{"content":"summary"}}`,
		`{"type":"user","isSidechain":true,"message":{"content":"sidechain text is skipped"}}`,
		`{"type":"api-request","params":{"messages":"skipped"}}`,
		`not json`,
	}
	path = filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, hook, hand
}

// assertContextRows checks every row's bytes against want and returns the sum of the rows.
func assertContextRows(t *testing.T, rep *ContextReport, want map[string]int64) (total int64) {
	t.Helper()
	got := map[string]int64{}
	for _, r := range rep.Rows {
		got[r.Source] = r.Bytes
		total += r.Bytes
	}
	if len(got) != len(want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	for src, n := range want {
		if got[src] != n {
			t.Errorf("%s = %d bytes, want %d", src, got[src], n)
		}
	}
	return total
}

// One synthetic transcript exercises every source: each row, the top entries, the token rule and the skipped lines.
func TestContextBySource(t *testing.T) {
	path, hook, hand := writeContextFixture(t)
	rep, err := ContextBySource(path)
	if err != nil {
		t.Fatal(err)
	}
	total := assertContextRows(t, rep, map[string]int64{
		"user text": 10 + 32, "assistant text": 20, "tool call: Bash": 12, "tool result: Bash": 50, "tool result: unknown": 3,
		"hook UserPromptSubmit memory-recall": int64(len(hook)), "task notifications": 10, "reminder: output_style": 7,
		"subagent hand-backs": int64(len(hand)), "compaction summary": 7,
	})
	if rep.Rows[0].Source != "subagent hand-backs" || rep.TotalBytes != total || rep.TotalTokens != (total+3)/4 {
		t.Errorf("order/total wrong: first %q total %d (rows sum %d) tokens %d",
			rep.Rows[0].Source, rep.TotalBytes, total, rep.TotalTokens)
	}
	top := rep.Top[0]
	if len(rep.Top) != 5 || top.Source != "subagent hand-backs" || top.Line != 11 || top.Tokens != EstimateTokens(top.Bytes) {
		t.Errorf("top = %+v", rep.Top)
	}
}

// quote is s as a JSON string literal.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A session id resolves in any project; with none given the newest of the working directory's project wins.
func TestSessionFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "projects", "-other"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "projects", "-other", "abc.jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := SessionFile("/cwd", "abc"); err != nil || got != want {
		t.Errorf("SessionFile(abc) = %q, %v; want %q", got, err, want)
	}
	if _, err := SessionFile("/cwd", "missing"); err == nil {
		t.Error("an unknown session must fail")
	}
	if _, err := SessionFile("/cwd", ""); err == nil || !strings.Contains(err.Error(), "--session") {
		t.Errorf("no session in the project: err = %v, want a --session hint", err)
	}
}
