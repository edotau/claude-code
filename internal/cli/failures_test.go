package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/worktree"
)

// TestFailuresVerb: report, ls, a second claim exits 3 naming the first, done hides it, a bad kind exits 2.
func TestFailuresVerb(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	_, out, _ := runCLI(t, "failures", "report", "--kind", "bench", "--subject", "pkg x", "--summary", "boom", "--log", "/tmp/l")
	id := strings.TrimSpace(strings.TrimPrefix(out, "reported "))
	_, ls, _ := runCLI(t, "failures", "ls", "--json")
	var rows []failureRow
	if err := json.Unmarshal([]byte(ls), &rows); err != nil || len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("ls --json = %q (%v), want one row with id %q", ls, err, id)
	}
	for _, tc := range []struct {
		args     []string
		code     int
		stdout   string
		stderr   string
		wantName string
	}{
		{[]string{"report", "--kind", "nope", "--subject", "x"}, 2, "", "not one of", "bad kind"},
		{[]string{"report", "--kind", "bench", "--subject", "pkg x"}, 0, "duplicate " + id, "", "repeat"},
		{[]string{"claim", id, "--session", "a"}, 0, "claude-code agents ask", "", "first claim"},
		{[]string{"claim", id, "--session", "b"}, 3, "", "held by a", "second claim"},
		{[]string{"ls"}, 0, "claimed by a", "", "ls claimed"},
		{[]string{"claim", "zzzzzzzzzzzz"}, 2, "", "no failure", "unknown id"},
		{[]string{"done", id, "other"}, 2, "", "exactly one", "extra id"},
		{[]string{"done", id}, 0, "done " + id, "", "done"},
		{[]string{"ls", "--all"}, 0, "[done]", "", "ls --all"},
		{[]string{"ls", "extra"}, 2, "", "no arguments", "ls positional"},
		{[]string{"report", "--kind", "audit", "--subject", "$(id)"}, 0, "reported", "", "shell subject"},
		{[]string{"claim", id, "--session", "c"}, 2, "", "already done", "claim of done"},
		{[]string{"report", "--kind", "bench", "--subject", "  "}, 2, "", "1 to 200", "blank subject"},
	} {
		code, so, se := runCLI(t, append([]string{"failures"}, tc.args...)...)
		if code != tc.code || !strings.Contains(so, tc.stdout) || !strings.Contains(se, tc.stderr) ||
			(tc.stdout == "" && so != "") || (tc.stderr == "" && se != "") {
			t.Errorf("%s: got %d %q %q, want %d %q %q", tc.wantName, code, so, se, tc.code, tc.stdout, tc.stderr)
		}
	}
	if _, so, _ := runCLI(t, "failures", "ls"); strings.Contains(so, id) {
		t.Errorf("ls after done = %q, want no row for %s", so, id)
	}
}

// TestFailureFixBriefFile: an audit failure's fix reads a generated brief file that names the log.
func TestFailureFixBriefFile(t *testing.T) {
	cmd, _, err := failureFix(worktree.Failure{Kind: worktree.FailAudit, Subject: "x", Log: "/tmp/l.log"})
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimSpace(cmd[strings.Index(cmd, "< ")+2:])
	path = strings.Trim(path, "'")
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "/tmp/l.log") {
		t.Fatalf("brief %q = %q, %v; want the log path", path, raw, err)
	}
	_ = os.Remove(path)
}
