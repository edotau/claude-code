package transcript

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLastUsageAndEdits(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	big := `{"type":"user","message":{"content":"` + strings.Repeat("z", tailBytes+10) + `"}}`
	lines := []string{
		`{"type":"assistant","message":{"model":"claude-opus-5","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":40}}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/b.go"}},{"type":"tool_use","name":"Read","input":{"file_path":"/r/skip.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/r/a.go"}}]}}`,
		big,
	}
	_ = os.WriteFile(tr, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	u, ok := LastUsage(tr)
	if !ok || u.Tokens != 100 || u.Model != "claude-opus-5" {
		t.Errorf("full-scan fallback: %+v %v", u, ok)
	}
	if got := EditedFiles(tr, 10); strings.Join(got, ",") != "/r/a.go,/r/b.go" {
		t.Errorf("edits %v", got)
	}
	if n := CountEdits(tr); n != 2 {
		t.Errorf("CountEdits = %d, want 2 (Read excluded)", n)
	}
	if _, ok := LastUsage(filepath.Join(t.TempDir(), "missing")); ok {
		t.Error("missing transcript reported usage")
	}
}

func TestWindow(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "claude-opus-5[1m]")
	for _, c := range []struct {
		model string
		used  int
		want  int
	}{{"claude-opus-5", 10, 1_000_000}, {"claude-haiku-4-5", 10, 200_000}, {"claude-haiku-4-5", 300_000, 1_000_000}, {"x[1m]", 1, 1_000_000}} {
		if got := Window(c.model, c.used); got != c.want {
			t.Errorf("Window(%q,%d)=%d want %d", c.model, c.used, got, c.want)
		}
	}
}

func appendTranscript(t *testing.T, path, chunk string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(chunk); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func checkEditsFrom(t *testing.T, path string, offset int64, wantEdits int, wantNext int64) {
	t.Helper()
	edits, next := CountEditsFrom(path, offset)
	if edits != wantEdits || next != wantNext {
		t.Fatalf("CountEditsFrom(offset=%d) = (%d, %d), want (%d, %d)",
			offset, edits, next, wantEdits, wantNext)
	}
	for range 2 {
		if edits, next := CountEditsFrom(path, wantNext); edits != 0 || next != wantNext {
			t.Fatalf("repeated read at offset %d = (%d, %d), want (0, %d)",
				wantNext, edits, next, wantNext)
		}
	}
}

func TestCountEditsFromIncremental(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incremental.jsonl")
	records := []struct {
		name  string
		line  string
		edits int
	}{
		{"empty", "", 0},
		{"edit", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/é.go"}}]}}`, 1},
		{"all editors", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/é.go"}},{"type":"tool_use","name":"Write","input":{"file_path":"/repo/new.go"}},{"type":"tool_use","name":"MultiEdit","input":{"file_path":"/repo/multi.go"}},{"type":"tool_use","name":"NotebookEdit","input":{"notebook_path":"/repo/test.ipynb"}},{"type":"tool_use","name":"Read","input":{"file_path":"/repo/read.go"}}]}}`, 4},
		{"user", `{"type":"user","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/ignored.go"}}]}}`, 0},
		{"read", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/repo/ignored.go"}}]}}`, 0},
		{"not tool use", `{"type":"assistant","message":{"content":[{"type":"text","name":"Edit","text":"tool_use"}]}}`, 0},
		{"text content", `{"type":"assistant","message":{"content":"tool_use Edit"}}`, 0},
		{"malformed", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit"}]}`, 0},
		{"malformed content", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":"bad"}]}}`, 0},
		{"null", `null`, 0},
		{"no record type", `{"message":{"content":[{"type":"tool_use","name":"Write","input":{}}]}}`, 0},
		{"missing path", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{}}]}}`, 1},
		{"after ignored records", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/é.go"}}]}}`, 1},
	}
	var offset int64
	total := 0
	for _, record := range records {
		t.Run(record.name, func(t *testing.T) {
			chunk := record.line + "\n"
			appendTranscript(t, path, chunk)
			next := offset + int64(len(chunk))
			checkEditsFrom(t, path, offset, record.edits, next)
			total += record.edits
			if got := CountEdits(path); got != total {
				t.Fatalf("accumulated edits = %d, full scan = %d", total, got)
			}
			offset = next
		})
	}
}

func TestCountEditsFromPartialRecords(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n"} {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(ending, "\r", "CR"), "\n", "LF"), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "partial.jsonl")
			edit := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/é.go"}}]}}`
			appendTranscript(t, path, edit+ending)
			offset := int64(len(edit + ending))
			checkEditsFrom(t, path, 0, 1, offset)

			appendTranscript(t, path, edit)
			checkEditsFrom(t, path, offset, 0, offset)
			if got := CountEdits(path); got != 1 {
				t.Fatalf("full scan with unterminated JSON = %d, want 1 complete-record edit", got)
			}
			appendTranscript(t, path, ending)
			checkEditsFrom(t, path, offset, 1, offset*2)
			offset *= 2

			cut := len(edit) / 2
			appendTranscript(t, path, edit[:cut])
			checkEditsFrom(t, path, offset, 0, offset)
			appendTranscript(t, path, edit[cut:])
			checkEditsFrom(t, path, offset, 0, offset)
			appendTranscript(t, path, ending)
			checkEditsFrom(t, path, offset, 1, offset+int64(len(edit+ending)))
			if got := CountEdits(path); got != 3 {
				t.Fatalf("full scan after completion = %d, want 3", got)
			}
		})
	}
}

func TestCountEditsFromTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncated.jsonl")
	edit := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/a.go"}}]}}` + "\n"
	appendTranscript(t, path, strings.Repeat(edit, 3))
	offset := int64(3 * len(edit))
	checkEditsFrom(t, path, 0, 3, offset)
	if err := os.WriteFile(path, []byte(edit), 0o600); err != nil {
		t.Fatal(err)
	}
	checkEditsFrom(t, path, offset, 1, int64(len(edit)))
	if got := CountEdits(path); got != 1 {
		t.Fatalf("full scan after truncation = %d, want 1", got)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	checkEditsFrom(t, path, int64(len(edit)), 0, 0)
	appendTranscript(t, path, edit)
	checkEditsFrom(t, path, 0, 1, int64(len(edit)))
}

func TestCountEditsFromLongLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long.jsonl")
	payload := strings.Repeat("x", (1<<20)+257)
	user := `{"type":"user","message":{"content":"` + payload + `"}}` + "\n"
	edit := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/repo/large.txt","content":"` + payload + `"}}]}}`
	appendTranscript(t, path, user)
	offset := int64(len(user))
	checkEditsFrom(t, path, 0, 0, offset)
	appendTranscript(t, path, edit[:1<<20])
	checkEditsFrom(t, path, offset, 0, offset)
	appendTranscript(t, path, edit[1<<20:])
	checkEditsFrom(t, path, offset, 0, offset)
	appendTranscript(t, path, "\n")
	next := offset + int64(len(edit)+1)
	checkEditsFrom(t, path, offset, 1, next)
	short := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/after.go"}}]}}` + "\n"
	appendTranscript(t, path, short)
	checkEditsFrom(t, path, next, 1, next+int64(len(short)))
	if got := CountEdits(path); got != 2 {
		t.Fatalf("full scan after long records = %d, want 2", got)
	}
}

// bashLine builds a single-tool_use assistant record running cmd under Bash.
func bashLine(cmd string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":%q}}]}}`, cmd)
}

// editLine builds a single-tool_use assistant Edit record whose old_string is oldStr.
func editLine(oldStr string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go","old_string":%q}}]}}`, oldStr)
}

// TestCountEditsFromGitCommitDetection: a segment counts only when git is actually RUN with the verb
// `commit` (including -C/-c flags and a leading env-var assignment before `git`), never on quoted/echoed
// text that merely mentions "git commit", and never twice when an Edit's own content mentions it.
func TestCountEditsFromGitCommitDetection(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		count bool // true: counts CommitEditWeight; false: counts 0
	}{
		{"plain git commit", bashLine("git commit -m x"), true},
		{"dash-C before the verb", bashLine("git -C /repo commit"), true},
		{"dash-c config before the verb", bashLine("git -c user.name=x commit"), true},
		{"leading env var assignment", bashLine("X=1 git commit"), true},
		{"grep for the quoted phrase", bashLine(`grep -R 'git commit' .`), false},
		{"echo of the quoted phrase", bashLine(`echo "git commit"`), false},
		{"bare commit, no git", bashLine("echo commit"), false},
		{"Edit content mentioning git commit", editLine("says git commit here"), false},
		// Heredoc bodies: a commit mentioned only in the body text must never count as run.
		{"quoted heredoc body", bashLine("cat <<'EOF'\ngit commit -m x\nEOF"), false},
		{"unquoted heredoc body", bashLine("cat <<EOF\ngit commit -m x\nEOF"), false},
		{"dash heredoc body", bashLine("cat <<-EOF\n\tgit commit -m x\n\tEOF"), false},
		{"here-string stays false (not a heredoc body to strip)", bashLine("cat <<< 'git commit -m x'"), false},
		// $(...)/backtick substitutions: a captured git commit still runs.
		{"dollar-paren substitution runs commit", bashLine("x=$(git commit -m x)"), true},
		{"backtick substitution runs commit", bashLine("x=`git commit -m x`"), true},
		{"nested substitution runs commit", bashLine("x=$(echo $(git commit -m x))"), true},
		{"substitution of quoted text stays false", bashLine(`x=$(echo 'git commit -m x')`), false},
		// Short-circuit global flags: these print something and exit before any verb (incl. commit) runs.
		{"--version short-circuits", bashLine("git --version commit"), false},
		{"--help short-circuits", bashLine("git --help commit"), false},
		{"-h short-circuits", bashLine("git -h commit"), false},
		{"bare --exec-path short-circuits", bashLine("git --exec-path commit"), false},
		{"--exec-path= supplies a value, does not short-circuit", bashLine("git --exec-path=/x commit"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "commit.jsonl")
			if err := os.WriteFile(path, []byte(tc.line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.count {
				want = CommitEditWeight
			}
			if tc.name == "Edit content mentioning git commit" {
				want = 1 // the Edit tool_use itself still counts as one ordinary edit
			}
			if got := CountEdits(path); got != want {
				t.Errorf("CountEdits(%q) = %d, want %d", tc.line, got, want)
			}
		})
	}
}

func TestCountEditsFromRealTranscript(t *testing.T) {
	// Secret-screened records 595-596 of session 849f0f6e-bed2-431c-9f5c-b53a09bcce17, embedded to keep file scope.
	data := []byte(`{"parentUuid":"21114221-039d-4d0f-a632-59b8d60c140b","isSidechain":false,"message":{"model":"claude-opus-5-5","id":"msg_011CgCAXxU1JFQecxcPvbG1D","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01Ap9PayWGw2qiashnyLQCHu","name":"Edit","input":{"replace_all":false,"file_path":"/home/edotau/.claude/.gitignore","old_string":"!/skills/using-git-worktrees\n","new_string":"!/skills/using-git-worktrees\n!/skills/web-search\n"},"caller":{"type":"direct"}}],"container":null,"stop_reason":"tool_use","stop_sequence":null,"stop_details":null,"usage":{"input_tokens":2,"cache_creation_input_tokens":974,"cache_read_input_tokens":116276,"output_tokens":319,"output_tokens_details":{"thinking_tokens":73},"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":974,"ephemeral_5m_input_tokens":0},"inference_geo":"global","iterations":[{"input_tokens":2,"output_tokens":319,"cache_read_input_tokens":116276,"cache_creation_input_tokens":974,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":974},"type":"message"}],"speed":"standard","fallback_credit":null},"input_transformations":[],"diagnostics":null,"context_management":null},"wireToolInputs":{"toolu_01Ap9PayWGw2qiashnyLQCHu":{"file_path":"/home/edotau/.claude/.gitignore","old_string":"!/skills/using-git-worktrees\n","new_string":"!/skills/using-git-worktrees\n!/skills/web-search\n"}},"serverClassifierRequest":"85c186fe-8354-403c-9920-20c818e7016d","apiBlockIndex":1,"requestId":"req_011CgCAXx9QEnMSJaaYi2b6g","requestRef":"15e60955-4f85-49cb-ad68-00c0c52e99f6","requestedModel":"claude-opus-5-5[1m]","type":"assistant","uuid":"5d78087f-16f0-4ce4-9149-24f695eb4a03","timestamp":"2026-10-19T19:41:29.785Z","advisorModel":"claude-opus-5-5","effort":"medium","perTurnEffort":"medium","session_id":"849f0f6e-bed2-431c-9f5c-b53a09bcce17","userType":"external","entrypoint":"cli","cwd":"/home/edotau/.claude","sessionId":"849f0f6e-bed2-431c-9f5c-b53a09bcce17","version":"2.1.303","gitBranch":"main"}
{"parentUuid":"5d78087f-16f0-4ce4-9149-24f695eb4a03","isSidechain":false,"message":{"model":"claude-opus-5-5","id":"msg_011CgCAXxU1JFQecxcPvbG1D","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01X3KAoinqWd99wB92K6cX6i","name":"Bash","input":{"command":"cd /home/edotau/.claude; git status --short skills .gitignore","description":"Confirm web-search is now visible to git"},"caller":{"type":"direct"}}],"container":null,"stop_reason":"tool_use","stop_sequence":null,"stop_details":null,"usage":{"input_tokens":2,"cache_creation_input_tokens":974,"cache_read_input_tokens":116276,"output_tokens":319,"output_tokens_details":{"thinking_tokens":73},"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":974,"ephemeral_5m_input_tokens":0},"inference_geo":"global","iterations":[{"input_tokens":2,"output_tokens":319,"cache_read_input_tokens":116276,"cache_creation_input_tokens":974,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":974},"type":"message"}],"speed":"standard","fallback_credit":null},"input_transformations":[],"diagnostics":null,"context_management":null},"wireToolInputs":{"toolu_01X3KAoinqWd99wB92K6cX6i":{"command":"cd /home/edotau/.claude; git status --short skills .gitignore","description":"Confirm web-search is now visible to git"}},"serverClassifierRequest":"85c186fe-8354-403c-9920-20c818e7016d","apiBlockIndex":2,"requestId":"req_011CgCAXx9QEnMSJaaYi2b6g","requestRef":"15e60955-4f85-49cb-ad68-00c0c52e99f6","requestedModel":"claude-opus-5-5[1m]","type":"assistant","uuid":"26120abe-3296-4b5b-bd0d-119ab0a5faca","timestamp":"2026-10-19T19:41:29.864Z","advisorModel":"claude-opus-5-5","effort":"medium","perTurnEffort":"medium","session_id":"849f0f6e-bed2-431c-9f5c-b53a09bcce17","userType":"external","entrypoint":"cli","cwd":"/home/edotau/.claude","sessionId":"849f0f6e-bed2-431c-9f5c-b53a09bcce17","version":"2.1.303","gitBranch":"main"}
`)
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatal("real transcript fixture must contain complete JSONL records")
	}
	path := filepath.Join(t.TempDir(), "real.jsonl")
	var offset int64
	total := 0
	for i, line := range bytes.SplitAfter(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		appendTranscript(t, path, string(line))
		edits, next := CountEditsFrom(path, offset)
		if want := offset + int64(len(line)); next != want {
			t.Fatalf("real record %d: next = %d, want %d", i, next, want)
		}
		total += edits
		if full := CountEdits(path); total != full {
			t.Fatalf("real record %d: accumulated edits = %d, full scan = %d", i, total, full)
		}
		checkEditsFrom(t, path, next, 0, next)
		offset = next
	}
	if total != 1 {
		t.Fatalf("real fixture counted %d edits, want 1", total)
	}
}
