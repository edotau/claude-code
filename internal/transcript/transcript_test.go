package transcript

import (
	"bytes"
	"encoding/json"
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
	if n := countEdits(tr); n != 2 {
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

// A Databricks pin "databricks-<id>[1m]" must cover the bare id the transcript records.
func TestWindowDatabricksPrefixedPin(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "databricks-claude-opus-5-5[1m]")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "")
	if got := Window("claude-opus-5-5", 182_796); got != 1_000_000 {
		t.Errorf("Window(claude-opus-5-5)=%d want 1000000", got)
	}
	if got := Window("claude-haiku-4-5", 10); got != 200_000 {
		t.Errorf("Window(claude-haiku-4-5)=%d want 200000 (unpinned model)", got)
	}
}

// A pin on another vendor's "vendor-<id>[1m]" must NOT hand the bare id a 1M window (only "databricks-" is stripped).
func TestWindowNonDatabricksPrefixedPinNotMatched(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "vendor-gpt-5[1m]")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "")
	if got := Window("gpt-5", 10); got == 1_000_000 {
		t.Errorf("Window(gpt-5)=%d: a vendor-gpt-5[1m] pin must not match bare gpt-5", got)
	}
	t.Setenv("ANTHROPIC_MODEL", "databricks-gpt-5[1m]")
	if got := Window("gpt-5", 10); got != 1_000_000 {
		t.Errorf("databricks- pin must still match: %d", got)
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
			if got := countEdits(path); got != total {
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
			if got := countEdits(path); got != 1 {
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
			if got := countEdits(path); got != 3 {
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
	if got := countEdits(path); got != 1 {
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
	if got := countEdits(path); got != 2 {
		t.Fatalf("full scan after long records = %d, want 2", got)
	}
}

// bashLine builds a single-tool_use assistant record running cmd under Bash.
func bashLine(cmd string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":%q}}]}}`, cmd)
}

// quoteArg wraps s in double quotes, escaping backslashes and quotes so it survives one more
// pass of commitShellTokens as a literal -c argument (mirrors real shell double-quote escaping).
func quoteArg(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// bashCWrap wraps inner as the quoted argument of a fresh `bash -c` layer.
func bashCWrap(inner string) string {
	return "bash -c " + quoteArg(inner)
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
		// Dangling/truncated substitutions must not panic and must not count as a commit.
		{"dangling dollar-paren after a comment", bashLine("echo commit # $("), false},
		{"bare dangling dollar-paren", bashLine("$("), false},
		{"nested dangling dollar-parens", bashLine("a $( b $( c"), false},
		{"dangling backtick", bashLine("echo `git commit"), false},
		{"dangling heredoc marker", bashLine("cat <<"), false},
		{"dangling quoted heredoc marker", bashLine("cat <<'"), false},
		{"complete substitution still runs commit", bashLine("echo $(git commit -m x)"), true},
		// New wrappers (R1): sudo flags, time/command/exec/nohup, `!`, and keyword-started segments.
		{"sudo dash-u user", bashLine("sudo -u deploy git commit -m x"), true},
		{"sudo dash-E", bashLine("sudo -E git commit -m x"), true},
		{"time wrapper", bashLine("time git commit -m x"), true},
		{"command wrapper", bashLine("command git commit -m x"), true},
		{"exec wrapper", bashLine("exec git commit -m x"), true},
		{"nohup wrapper", bashLine("nohup git commit -m x"), true},
		{"bang negation wrapper", bashLine("! git commit -m x"), true},
		{"then starts a fresh segment", bashLine("if true; then git commit -m x; fi"), true},
		{"do starts a fresh segment", bashLine("for x in 1; do git commit -m x; done"), true},
		{"else starts a fresh segment", bashLine("if false; then echo no; else git commit -m x; fi"), true},
		{"elif starts a fresh segment", bashLine("if false; then echo no; elif true; then git commit -m x; fi"), true},
		{"combined: sudo + time + then", bashLine("if true; then sudo time git commit -m x; fi"), true},
		{"combined: nohup + sudo dash-g", bashLine("nohup sudo -g admin git commit -m x"), true},
		// New value-options (R1): --namespace/--super-prefix/--config-env, both = and space forms.
		{"--namespace space form", bashLine("git --namespace foo commit -m x"), true},
		{"--namespace= form", bashLine("git --namespace=foo commit -m x"), true},
		{"--super-prefix space form", bashLine("git --super-prefix foo commit -m x"), true},
		{"--super-prefix= form", bashLine("git --super-prefix=foo commit -m x"), true},
		{"--config-env space form", bashLine("git --config-env foo commit -m x"), true},
		{"--config-env= form", bashLine("git --config-env=foo commit -m x"), true},
		// Backslash escapes and line continuations (R2): an escaped quote never closes early, and
		// `\`+newline joins the next line into the same segment.
		{"backslash-escaped quote keeps the segment open", bashLine(`echo "a \"; git commit"`), false},
		{"backslash-newline continuation", bashLine("git \\\ncommit -m x"), true},
		// `bash`/`sh`/`zsh -c "<script>"` (R2): the quoted script runs as a fresh shell command line,
		// bounded by maxShellRecursionDepth so nesting it past the cap never counts and never hangs.
		{"bash -c wraps a commit", bashLine(`bash -c "git commit -m x"`), true},
		{"sh -c wraps a commit", bashLine(`sh -c "git commit -m x"`), true},
		{"zsh -c wraps a commit", bashLine(`zsh -c "git commit -m x"`), true},
		{"/bin/bash -c wraps a commit", bashLine(`/bin/bash -c "git commit -m x"`), true},
		{"sh -c with only a quoted mention", bashLine(`sh -c "echo git commit"`), false},
		{"nested bash -c two layers deep still counts", bashLine(bashCWrap(bashCWrap("git commit -m x"))), true},
		{
			"nested bash -c four layers deep exceeds the cap",
			bashLine(bashCWrap(bashCWrap(bashCWrap(bashCWrap("git commit -m x"))))),
			false,
		},
		// `git -c alias.<name>=commit <name>` (R2): the alias must resolve to exactly `commit`, and the
		// invoked verb must match the alias name, not just any alias being present.
		{"alias.ci=commit, invoked as ci", bashLine("git -c alias.ci=commit ci -m x"), true},
		{"alias.ci=status, invoked as ci", bashLine("git -c alias.ci=status ci -m x"), false},
		{"alias.ci=commit, invoked as status", bashLine("git -c alias.ci=commit status -m x"), false},
		// Double-quote escape rule: a backslash before a non-escapable char stays literal, so the
		// quoted token keeps its backslash and never equals "commit".
		{"backslash before non-escapable char in dquotes stays literal", bashLine(`git "comm\it"`), false},
		// Combined short flags and -n (no-exec): -xc/-ec count as -c; -n anywhere blocks execution.
		{"bash -xc combined flag still finds -c", bashLine(`bash -xc "git commit -m x"`), true},
		{"bash -n -c is no-exec, never runs", bashLine(`bash -n -c "git commit -m x"`), false},
		// Repeated -c alias.<name>=<val>: the last assignment for a given name wins.
		{"alias.ci=commit then alias.ci=status: last wins", bashLine("git -c alias.ci=commit -c alias.ci=status ci -m x"), false},
		// A builtin verb name ignores a user alias registered under it; status stays status, not commit.
		{"alias.status=commit, invoked as status: builtin alias ignored", bashLine("git -c alias.status=commit status -m x"), false},
		// A shell alias (`!<cmd>`) recurses into its own command text, depth-limited.
		{"alias.ci='!git commit' shell alias runs commit", bashLine(`git -c alias.ci='!git commit' ci -m x`), true},
		// env's own flags are skipped before the NAME=value assignments and the wrapped command.
		{"env -i PATH=/x git commit reaches git", bashLine("env -i PATH=/x git commit -m x"), true},
		// Non-edit, non-commit lines must contribute 0 (no accidental decode-time double count).
		{"Read line", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/r/a.go"}}]}}`, false},
		{"Grep line", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"commit"}}]}}`, false},
		{"Bash without commit", bashLine("echo hello"), false},
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
			if got := countEdits(path); got != want {
				t.Errorf("countEdits(%q) = %d, want %d", tc.line, got, want)
			}
		})
	}
}

// TestCountLineEditsOneContentArrayBothEditAndCommitCountOnce: a single assistant record whose content
// array holds both an Edit tool_use and a Bash `git commit` tool_use must count each exactly once —
// countLineEdits decodes the line once and walks every content item, so neither is missed or doubled.
func TestCountLineEditsOneContentArrayBothEditAndCommitCountOnce(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go"}},` +
		`{"type":"tool_use","name":"Bash","input":{"command":"git commit -m x"}}` +
		`]}}`
	path := filepath.Join(t.TempDir(), "both.jsonl")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := 1 + CommitEditWeight
	if got := countEdits(path); got != want {
		t.Errorf("countEdits(Edit+commit in one record) = %d, want %d", got, want)
	}
}

// TestCountLineEditsMalformedSubstitutionWithEditStillCountsEdit: a Bash tool_use with a dangling `$(`
// never actually panics (extractSubstitutions returns what it found so far; see
// TestExtractSubstitutionsDanglingOpen), so a sibling Edit tool_use in the same content array must still
// count and the malformed command itself must not count as a commit. countLineEdits's whole-line recover
// is therefore never exercised by this input — this test pins that no-panic path, not the recover itself.
func TestCountLineEditsMalformedSubstitutionWithEditStillCountsEdit(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go"}},` +
		`{"type":"tool_use","name":"Bash","input":{"command":"echo commit # $("}}` +
		`]}}`
	path := filepath.Join(t.TempDir(), "malformed-with-edit.jsonl")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := countEdits(path); got != 1 {
		t.Errorf("countEdits(malformed $( alongside an Edit) = %d, want 1 (the Edit counts; the dangling $( never counts as a commit)", got)
	}
}

// TestCountEditsFromSequentialCallsIndependentWithPool: the pooled bufio.Reader must not leak state
// (a stale long-line buffer, a wrong read position) across independent CountEditsFrom calls on different
// files, even when run back-to-back so the pool is likely to hand back the same *bufio.Reader.
func TestCountEditsFromSequentialCallsIndependentWithPool(t *testing.T) {
	dir := t.TempDir()
	editLn := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go"}}]}}` + "\n"
	for round := range 20 {
		pathA := filepath.Join(dir, fmt.Sprintf("a-%d.jsonl", round))
		pathB := filepath.Join(dir, fmt.Sprintf("b-%d.jsonl", round))
		if err := os.WriteFile(pathA, []byte(strings.Repeat(editLn, 3)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pathB, []byte(strings.Repeat(editLn, 5)), 0o600); err != nil {
			t.Fatal(err)
		}
		editsA, nextA := CountEditsFrom(pathA, 0)
		editsB, nextB := CountEditsFrom(pathB, 0)
		if editsA != 3 || nextA != int64(3*len(editLn)) {
			t.Fatalf("round %d: file A = (%d,%d), want (3,%d)", round, editsA, nextA, 3*len(editLn))
		}
		if editsB != 5 || nextB != int64(5*len(editLn)) {
			t.Fatalf("round %d: file B = (%d,%d), want (5,%d)", round, editsB, nextB, 5*len(editLn))
		}
	}
}

// TestExtractSubstitutionsDanglingOpen: an unterminated `$(` must return what was found so far, never panic.
func TestExtractSubstitutionsDanglingOpen(t *testing.T) {
	cases := []struct{ name, cmd string }{
		{"bare dangling", "$("},
		{"trailing comment dangling", "echo commit # $("},
		{"nested dangling", "a $( b $( c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractSubstitutions(tc.cmd); len(got) != 0 {
				t.Errorf("extractSubstitutions(%q) = %v, want empty", tc.cmd, got)
			}
		})
	}
}

// TestCountEditsFromMalformedCommandNoPanic: a Bash command with a dangling `$(` must not panic
// CountEditsFrom and must not count as a commit — the recover in eachCommitLine is the safety net.
func TestCountEditsFromMalformedCommandNoPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commit.jsonl")
	line := bashLine("echo commit # $(")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := countEdits(path); got != 0 {
		t.Errorf("CountEdits with a dangling $( = %d, want 0", got)
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
		if full := countEdits(path); total != full {
			t.Fatalf("real record %d: accumulated edits = %d, full scan = %d", i, total, full)
		}
		checkEditsFrom(t, path, next, 0, next)
		offset = next
	}
	if total != 1 {
		t.Fatalf("real fixture counted %d edits, want 1", total)
	}
}

func TestContentText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"bare string", `"hello"`, "hello"},
		{"text and tool_use", `[{"type":"text","text":"a"},{"type":"tool_use","name":"X"},{"type":"text","text":"b"}]`, "ab"},
		{"empty array", `[]`, ""},
		{"invalid json", `{not json`, ""},
		{"empty input", ``, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ContentText(json.RawMessage(c.in)); got != c.want {
				t.Errorf("ContentText(%s) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestEachLineExportedHandlesHugeAndUnterminated(t *testing.T) {
	// Given a 5 MiB line and an unterminated tail; When iterated; Then every line arrives whole
	giant := strings.Repeat("x", 5<<20) + "\n"
	var got []string
	EachLine(strings.NewReader("a\n"+giant+"tail"), func(l []byte) { got = append(got, string(l)) })
	want := []string{"a\n", giant, "tail"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: len %d, want %d", i, len(got[i]), len(want[i]))
		}
	}
}

// countEdits is the whole-file count: CountEditsFrom from offset 0.
func countEdits(path string) int {
	n, _ := CountEditsFrom(path, 0)
	return n
}
