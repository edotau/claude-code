package safety

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// gitPolicyCases pin worktree-git.md rules 1, 3 and 5: commit message shape, named-file staging, `&&` after a gate.
func gitPolicyCases(dir string) []safetyTierCase {
	const body = "feat(x): add y\n\nbody line\nBREAKING CHANGE: z"
	return []safetyTierCase{
		{`git commit -m "feat(hooks): add check"`, dir, core.Allow},
		{`git commit -m "fix!: drop flag" -m "BREAKING CHANGE: gone"`, dir, core.Allow},
		{`git commit -m "added a thing"`, dir, core.Block},
		{`git commit -am "Feat: caps"`, dir, core.Block},
		{`git commit -m "feat: a" -m "second" -m "third"`, dir, core.Block},
		{`git commit -m "feat: a" -m "second" -m "BREAKING CHANGE: x"`, dir, core.Allow},
		{`git -C ` + dir + ` commit -m "wip"`, "", core.Block},
		{`git -C ` + dir + ` commit -m "chore: ok"`, "", core.Allow},
		{"git commit -m \"$(cat <<'EOF'\n" + body + "\nEOF\n)\"", dir, core.Allow},
		{"git commit -m \"$(cat <<'EOF'\nupdate stuff\nEOF\n)\"", dir, core.Block},
		{"git commit -m \"$(cat <<'EOF'\nfeat: a\nline two\nline three\nEOF\n)\"", dir, core.Block},
		{"git commit -F - <<'EOF'\nfeat: a\nEOF", dir, core.Allow},
		{"git commit -F - <<'EOF'\nnot conventional\nEOF", dir, core.Block},
		{`git commit`, dir, core.Allow},
		{`git commit --no-edit`, dir, core.Allow},
		{`git commit -C HEAD`, dir, core.Allow},
		{`echo "git commit -m nope"`, dir, core.Allow},
		{`git add -A`, dir, core.Block},
		{`git add --all`, dir, core.Block},
		{`git add .`, dir, core.Block},
		{`git add :/`, dir, core.Block},
		{`git add sub/`, dir, core.Block},
		{`git add sub`, dir, core.Block},
		{`git -C ` + dir + ` add sub`, "", core.Block},
		{`git add a.txt b.txt`, dir, core.Allow},
		{`git add -- a.txt`, dir, core.Allow},
		{`git add -u a.txt`, dir, core.Allow},
		{`make lint && git push`, dir, core.Allow},
		{`make lint; git push`, dir, core.Allow},                 // rewritten to &&
		{"go test ./...\ngit push origin main", dir, core.Allow}, // rewritten to &&
		{`(make lint; git push)`, dir, core.Block},
		{`go test ./... && git push`, dir, core.Allow},
		{`git status; git push`, dir, core.Allow},
		{`make lint; git status`, dir, core.Allow},
	}
}

func TestSafetyScanGitPolicy(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range gitPolicyCases(dir) {
		res := Scan(core.ParseInput(tierPayload(c.cmd, c.cwd)))
		if res.Decision != c.want {
			t.Errorf("%q: got %d want %d (%s)", c.cmd, res.Decision, c.want, res.Reason)
		}
		if c.want == core.Block && strings.Contains(strings.TrimSpace(res.Reason), "\n") {
			t.Errorf("%q: reason is not one line: %q", c.cmd, res.Reason)
		}
	}
}

// gitFixCase is a command the guard fixes (want non-empty) or leaves to a deny (want empty).
type gitFixCase struct{ cmd, want string }

func gitFixCases() []gitFixCase {
	const foot = "Co-Authored-By: Claude <noreply@anthropic.com>"
	return []gitFixCase{
		{"make lint; git push", "make lint && git push"},
		{"go test ./...\ngit push origin main", "go test ./... && git push origin main"},
		{`make lint; echo "a;b"; make test; git push`, `make lint; echo "a;b"; make test && git push`},
		{`make lint; echo "x; git push"`, ""},
		{"(make lint; git push)", ""},
		{"make lint; git push <<EOF\nx\nEOF", ""},
		{`git commit -m "feat: x" -m "` + foot + `"`, `git commit -m "feat: x"`},
		{"git commit -m \"feat: x\n\n" + foot + "\"", "git commit -m \"feat: x\n\n\""},
		{"git commit -m \"$(cat <<'EOF'\nfeat: x\n\n" + foot + "\nEOF\n)\"", "git commit -m \"$(cat <<'EOF'\nfeat: x\n\nEOF\n)\""},
		{`git commit -F msg.txt`, ""},
		{`git commit -m "feat: x\n\n` + foot + `"`, ""}, // footer shares a line with the command
	}
}

func TestSafetyScanGitRewrite(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, c := range gitFixCases() {
		res := Scan(core.ParseInput(tierPayload(c.cmd, "")))
		got, _ := res.UpdatedInput["command"].(string)
		if got != c.want || (c.want != "" && (res.Decision != core.Allow || !strings.HasPrefix(res.SystemMessage, "safety: "))) {
			t.Errorf("%q: rewrote to %q (decision %d, note %q); want %q", c.cmd, got, res.Decision, res.SystemMessage, c.want)
		}
	}
}

func TestSafetyScanGitRewriteDeniesWithCommand(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	auto := `{"tool_name":"Bash","permission_mode":"auto","tool_input":{"command":"make lint; git push"}}`
	res := Scan(core.ParseInput(strings.NewReader(auto)))
	if res.Decision != core.Block || res.UpdatedInput != nil || !strings.HasSuffix(res.Reason, "resend exactly: make lint && git push") {
		t.Errorf("auto mode: %+v", res)
	}
	cop := `{"sessionId":"x","toolName":"Bash","toolArgs":{"command":"make lint; git push"}}`
	res = Scan(core.ParseInput(strings.NewReader(cop)))
	if res.Decision != core.Block || !strings.HasSuffix(res.Reason, "resend exactly: make lint && git push") {
		t.Errorf("copilot: %+v", res)
	}
}

func TestSafetyScanGitDenyNamesCommand(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for cmd, want := range map[string]string{
		`git commit -m "wip"`: `use: git commit -m "fix(<scope>): <summary>"`,
		`git add -A`:          "use: git add <file> <file>",
	} {
		res := Scan(core.ParseInput(tierPayload(cmd, "")))
		if res.Decision != core.Block || !strings.Contains(res.Reason, want) || len(res.Reason) > 80 || strings.Contains(res.Reason, "\n") {
			t.Errorf("%q: %q (%d)", cmd, res.Reason, len(res.Reason))
		}
	}
}
