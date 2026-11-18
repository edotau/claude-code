package wthook

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

// claimFixture is a repo with a.txt and b.txt committed, a config dir, and liveOwner as the only live pid.
func claimFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := paths.Real(hosttest.Repo(t))
	hosttest.WriteRel(t, repo, "a.txt", "a\n")
	hosttest.WriteRel(t, repo, "b.txt", "b\n")
	hosttest.In(t, repo, "add", ".")
	hosttest.In(t, repo, "commit", "-qm", "files")
	orig := claimAlive
	claimAlive = func(pid int) bool { return pid == liveOwner }
	t.Cleanup(func() { claimAlive = orig })
	return repo
}

// claimEdit records an Edit of rel by session (owner pid) and leaves rel dirty.
func claimEdit(t *testing.T, repo string, owner int, session, rel string) {
	t.Helper()
	useClaimOwner(t, owner)
	hosttest.WriteRel(t, repo, rel, "edited by "+session+"\n")
	Claim(claimInput(t, map[string]any{"session_id": session, "cwd": repo, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": rel}}))
	useClaimOwner(t, checkerOwner) // the checks that follow run as another process
}

// resetClaimCase forgets every recorded claim, so cases can share one fixture repo.
func resetClaimCase(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(worktree.ClaimsDir()); err != nil {
		t.Fatal(err)
	}
}

func claimCheckOn(t *testing.T, repo, session, tool string, input map[string]any) core.Result {
	t.Helper()
	useClaimOwner(t, checkerOwner) // the check runs as a process other than the claimants
	return ClaimCheck(claimInput(t, map[string]any{"session_id": session, "cwd": repo, "tool_name": tool,
		"tool_input": input}))
}

func TestClaimCheckGitPathspecs(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	for _, args := range []string{"'" + filepath.Join(repo, "a.txt") + "'", "'*.txt'", "':(top)a.txt'"} {
		r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add -- " + args})
		if r.Decision != core.Ask || !strings.Contains(r.Reason, "a.txt takes") {
			t.Errorf("git add -- %s: %v %q; want an ask for a.txt", args, r.Decision, r.Reason)
		}
	}
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add -- . ':(exclude)a.txt'"})
	if r.Decision != core.Allow {
		t.Fatalf("excluded peer file: %v %q; want allow", r.Decision, r.Reason)
	}
	if err := os.Remove(filepath.Join(repo, "a.txt")); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, repo, "add", "--", "a.txt")
	useClaimOwner(t, liveOwner)
	Claim(claimInput(t, map[string]any{"session_id": "deleter", "cwd": repo, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "git add -- a.txt"}}))
	useClaimOwner(t, checkerOwner)
	claims, err := liveClaims(&core.Input{SessionID: "A"})
	if err != nil || len(claims[filepath.Join(repo, "a.txt")]) != 1 {
		t.Fatalf("staged deletion lost its claim: %v, err = %v", claims, err)
	}
	r = claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git commit -m x -- a.txt"})
	if r.Decision != core.Ask {
		t.Fatalf("commit of staged deletion: %v %q; want ask", r.Decision, r.Reason)
	}
}

func TestClaimCheckQuotedStagedPath(t *testing.T) {
	repo := claimFixture(t)
	const name = "caf\u00e9.txt"
	hosttest.WriteRel(t, repo, name, "external edit\n")
	hosttest.In(t, repo, "-c", "core.quotePath=true", "add", "--", name)
	useClaimOwner(t, liveOwner)
	Claim(claimInput(t, map[string]any{"session_id": "A", "cwd": repo, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "git add -- ."}}))
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git -c core.quotePath=true commit -m x"})
	if r.Decision != core.Ask || !strings.Contains(r.Reason, name+" takes") {
		t.Fatalf("bare commit of quoted filename: %v %q; want an ask for %s", r.Decision, r.Reason, name)
	}
}

// TestClaimCheck: a live peer's dirty claim advises once and asks on staging; a dead owner's claim and file go.
func TestClaimCheck(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	claimEdit(t, repo, deadOwner, "D", "b.txt")
	edit := map[string]any{"file_path": filepath.Join(repo, "a.txt")}
	if r := claimCheckOn(t, repo, "B", "Edit", edit); !strings.Contains(r.Context, "a.txt has uncommitted edits by") {
		t.Fatalf("first edit of A's file: context %q; want the advisory", r.Context)
	}
	if r := claimCheckOn(t, repo, "B", "Edit", edit); r.Context != "" {
		t.Fatalf("repeat edit must be silent, got %q", r.Context)
	}
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add -A"})
	if r.Decision != core.Ask || !strings.Contains(r.Reason, "a.txt takes") || strings.Contains(r.Reason, "b.txt") {
		t.Fatalf("git add -A: %v %q; want an ask naming a.txt only (b.txt's owner is dead)", r.Decision, r.Reason)
	}
	if _, err := os.Stat(claimsFile(deadOwner, "D")); !os.IsNotExist(err) {
		t.Fatalf("dead owner's claims file must be removed, stat err = %v", err)
	}
	for _, cmd := range []string{"git add -p a.txt", "git add b.txt", "git status"} {
		if r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": cmd}); r.Decision != core.Allow {
			t.Fatalf("%s: %v %q; want allow", cmd, r.Decision, r.Reason)
		}
	}
}

// TestClaimCheckOwnPidNewSlug: after /clear or a resume the same pid files under a new slug; that is no peer.
func TestClaimCheckOwnPidNewSlug(t *testing.T) {
	for _, tc := range []struct {
		name      string
		checkPid  int
		wantAsked bool
	}{{"same pid, new session", liveOwner, false}, {"other pid, other session", checkerOwner, true}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := claimFixture(t)
			claimEdit(t, repo, liveOwner, "OLD", "a.txt")
			useClaimOwner(t, tc.checkPid)
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "NEW", "cwd": repo, "tool_name": "Bash",
				"tool_input": map[string]any{"command": "git add -A"}}))
			if asked := r.Decision == core.Ask; asked != tc.wantAsked {
				t.Fatalf("git add -A as pid %d: %v %q; want ask = %v", tc.checkPid, r.Decision, r.Reason, tc.wantAsked)
			}
		})
	}
}

// TestClaimCheckLeafBlocksOnSiblingClaim: a leaf staging a sibling leaf's claim asks; under ~/.claude it is blocked.
func TestClaimCheckLeafBlocksOnSiblingClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inCfg bool
		link  bool
		sub   bool
		want  core.Decision
	}{{"outside the config tree", false, false, false, core.Ask}, {"inside the config tree", true, false, false, core.Block},
		{"linked worktree of the config tree", false, true, false, core.Block},
		{"subdirectory of a linked worktree", false, true, true, core.Block}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := claimFixture(t)
			if tc.inCfg {
				t.Setenv("CLAUDE_CONFIG_DIR", filepath.Dir(repo))
			}
			if tc.link {
				t.Setenv("CLAUDE_CONFIG_DIR", repo)
				wt := filepath.Join(paths.Real(t.TempDir()), "wt")
				hosttest.In(t, repo, "worktree", "add", "-q", "-b", "linked", wt)
				repo = wt
			}
			useClaimOwner(t, liveOwner)
			tr := filepath.Join(t.TempDir(), "S.jsonl")
			hosttest.WriteRel(t, filepath.Dir(tr), "S/subagents/agent-leafone1.jsonl", "{}\n")
			hosttest.WriteRel(t, repo, "a.txt", "leaf one\n")
			Claim(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leafone1", "cwd": repo,
				"transcript_path": tr, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}))
			cwd := repo
			if tc.sub {
				cwd = filepath.Join(repo, "sub")
				hosttest.WriteRel(t, repo, "sub/f.txt", "x\n")
			}
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leaftwo2", "cwd": cwd,
				"transcript_path": tr, "tool_name": "Bash", "tool_input": map[string]any{"command": "git commit -am x"}}))
			if r.Decision != tc.want || !strings.Contains(r.Reason, "leaf leafone1") {
				t.Fatalf("sibling leaf commit -am: %v %q; want %v naming leaf leafone1", r.Decision, r.Reason, tc.want)
			}
		})
	}
}

// TestClaimCheckMainCommitsHandedBackLeaf: main asks on a live leaf's claim, passes once handed back.
func TestClaimCheckMainCommitsHandedBackLeaf(t *testing.T) {
	repo := claimFixture(t)
	useClaimOwner(t, liveOwner)
	tr := filepath.Join(t.TempDir(), "S.jsonl")
	leafTr := "S/subagents/agent-leafone1.jsonl"
	working := `{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"Edit"}]}}` + "\n"
	hosttest.WriteRel(t, filepath.Dir(tr), leafTr, working)
	hosttest.WriteRel(t, repo, "a.txt", "leaf one\n")
	Claim(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leafone1", "cwd": repo,
		"transcript_path": tr, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}))
	add := map[string]any{"session_id": "S", "cwd": repo, "transcript_path": tr, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "git add a.txt"}}
	if r := ClaimCheck(claimInput(t, add)); r.Decision != core.Ask {
		t.Fatalf("live leaf's file: %v %q; want ask", r.Decision, r.Reason)
	}
	handback := `{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_h1",` +
		`"name":"SubagentHandback"}]}}` + "\n"
	denied := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"PreToolUse:` +
		`SubagentHandback hook error: dispatch: BLOCKED — job running","is_error":true,"tool_use_id":"toolu_h1"}]},` +
		`"toolUseResult":"Error: PreToolUse:SubagentHandback hook error","toolDenialKind":"permission-rule"}` + "\n"
	for name, tail := range map[string]string{"pending": "", "denied": denied} {
		hosttest.WriteRel(t, filepath.Dir(tr), leafTr, working+handback+tail)
		if r := ClaimCheck(claimInput(t, add)); r.Decision != core.Ask {
			t.Fatalf("%s hand-back leaf's file: %v %q; want ask", name, r.Decision, r.Reason)
		}
	}
	delivered := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_h1","type":"tool_result",` +
		`"content":[{"type":"text","text":"{\"success\":true}"}]}]},"toolUseResult":{"success":true},"toolEndsTurn":true}` + "\n"
	hosttest.WriteRel(t, filepath.Dir(tr), leafTr, working+handback+delivered)
	if r := ClaimCheck(claimInput(t, add)); r.Decision != core.Allow {
		t.Fatalf("handed-back leaf's file: %v %q; want allow", r.Decision, r.Reason)
	}
}

// TestClaimCheckDropsLandedClaim: a claim older than the file's last commit has landed, so staging the file passes.
func TestClaimCheckDropsLandedClaim(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	hosttest.In(t, repo, "add", "a.txt")
	t.Setenv("GIT_COMMITTER_DATE", time.Now().Add(time.Hour).Format(time.RFC3339))
	hosttest.In(t, repo, "commit", "-qm", "A lands a.txt")
	hosttest.WriteRel(t, repo, "a.txt", "B's own edit\n")
	if r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add a.txt"}); r.Decision != core.Allow {
		t.Fatalf("landed claim: %v %q; want allow", r.Decision, r.Reason)
	}
}

// TestClaimCheckBashEditClaimedByAdd: a file changed outside Edit is claimed once staged; a peer's bare commit asks.
func TestClaimCheckBashEditClaimedByAdd(t *testing.T) {
	repo := claimFixture(t)
	useClaimOwner(t, liveOwner)
	hosttest.WriteRel(t, repo, "b.txt", "sed -i by A\n")
	hosttest.In(t, repo, "add", "b.txt")
	Claim(claimInput(t, map[string]any{"session_id": "A", "cwd": repo, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "git add b.txt"}, "tool_response": map[string]any{"stdout": ""}}))
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git commit -m x"})
	if r.Decision != core.Ask || !strings.Contains(r.Reason, "b.txt takes") {
		t.Fatalf("bare commit over A's staged b.txt: %v %q; want an ask naming b.txt", r.Decision, r.Reason)
	}
}

// The commit ask sizes the sweep: every staged file it takes, and how many of those a peer claims.
func TestClaimCheckCommitAskCountsWholeStage(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	hosttest.WriteRel(t, repo, "c.txt", "unclaimed\n")
	hosttest.WriteRel(t, repo, "d.txt", "unclaimed\n")
	hosttest.In(t, repo, "add", "a.txt", "c.txt", "d.txt")
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git commit -m x"})
	if r.Decision != core.Ask || !strings.Contains(r.Reason, "takes 3 file(s), 1 of them") {
		t.Fatalf("bare commit of 3 staged, 1 claimed: %v %q; want the ask to count 3 and 1", r.Decision, r.Reason)
	}
}

// TestClaimCheckCommitAskNamesSafeForm: add-then-bare-commit over a peer's staged file asks, naming the safe forms.
func TestClaimCheckCommitAskNamesSafeForm(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	hosttest.In(t, repo, "add", "a.txt")
	hosttest.WriteRel(t, repo, "b.txt", "B's own\n")
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add b.txt && git commit -q -m x"})
	safe := strings.Contains(r.Reason, "git commit -- <your files>") && strings.Contains(r.Reason, "claude-code worktree commit")
	if r.Decision != core.Ask || !strings.Contains(r.Reason, "a.txt takes") || !safe {
		t.Fatalf("add && bare commit: %v %q; want an ask naming a.txt and git commit --", r.Decision, r.Reason)
	}
	if r = claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git commit -m x -- b.txt"}); r.Decision != core.Allow {
		t.Fatalf("commit -- b.txt: %v %q; want allow", r.Decision, r.Reason)
	}
}

func TestClaimCheckCommitInteractiveFlags(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    core.Decision
	}{
		{"git commit -i -m x -- a.txt", core.Ask},
		{"git commit -m '-p' -- a.txt", core.Ask},
		{"git commit -p -m x -- a.txt", core.Allow},
		{"git add -i -- a.txt", core.Allow},
	} {
		t.Run(tc.command, func(t *testing.T) {
			repo := claimFixture(t)
			claimEdit(t, repo, liveOwner, "A", "a.txt")
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "B", "agent_id": "leafB", "cwd": repo,
				"tool_name": "Bash", "tool_input": map[string]any{"command": tc.command}}))
			if r.Decision != tc.want {
				t.Fatalf("%s: %v %q; want %v", tc.command, r.Decision, r.Reason, tc.want)
			}
			if tc.want == core.Ask && !strings.Contains(r.Reason, "a.txt takes") {
				t.Fatalf("block must name claimed a.txt, got %q", r.Reason)
			}
		})
	}
}

func TestClaimCheckCommitIncludeRetainsStaged(t *testing.T) {
	for _, flag := range []string{"--include", "-i"} {
		t.Run(flag, func(t *testing.T) {
			repo := claimFixture(t)
			claimEdit(t, repo, liveOwner, "A", "a.txt")
			hosttest.In(t, repo, "add", "a.txt")
			hosttest.WriteRel(t, repo, "b.txt", "B's own edit\n")
			command := "git commit " + flag + " -m x -- b.txt"
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "B", "agent_id": "leafB", "cwd": repo,
				"tool_name": "Bash", "tool_input": map[string]any{"command": command}}))
			if r.Decision != core.Ask || !strings.Contains(r.Reason, "a.txt takes") {
				t.Fatalf("%s: %v %q; want ask naming staged a.txt", command, r.Decision, r.Reason)
			}
			if !strings.Contains(r.Reason, "takes 2 file(s), 1 of them") {
				t.Fatalf("include must count staged a.txt and operand b.txt, got %q", r.Reason)
			}
		})
	}
}

func TestClaimCheckAddPathspecFromFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		option  string
		content string
		want    core.Decision
	}{
		{"lines", "--pathspec-from-file paths.txt", "a.txt\n", core.Ask},
		{"equals", "--pathspec-from-file=paths.txt", "a.txt\n", core.Ask},
		{"quoted", "--pathspec-from-file paths.txt", "\"a.txt\"\r\n", core.Ask},
		{"nul", "--pathspec-from-file paths.txt --pathspec-file-nul", "a.txt\x00", core.Ask},
		{"unclaimed", "--pathspec-from-file paths.txt", "b.txt\n", core.Allow},
		{"empty", "--pathspec-from-file paths.txt", "", core.Allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := claimFixture(t)
			claimEdit(t, repo, liveOwner, "A", "a.txt")
			hosttest.WriteRel(t, repo, "b.txt", "B's own edit\n")
			hosttest.WriteRel(t, repo, "paths.txt", tc.content)
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "B", "agent_id": "leafB", "cwd": repo,
				"tool_name": "Bash", "tool_input": map[string]any{"command": "git add " + tc.option}}))
			if r.Decision != tc.want {
				t.Fatalf("%s: %v %q; want %v", tc.option, r.Decision, r.Reason, tc.want)
			}
			if tc.want == core.Ask && !strings.Contains(r.Reason, "a.txt takes") {
				t.Fatalf("block must name claimed a.txt, got %q", r.Reason)
			}
		})
	}
}

func TestClaimCheckCommitPathspecFromFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		option  string
		content string
	}{
		{"lines", "--pathspec-from-file paths.txt", "a.txt\n"},
		{"equals", "--pathspec-from-file=paths.txt", "a.txt\n"},
		{"quoted", "--pathspec-from-file paths.txt", "\"a.txt\"\r\n"},
		{"nul", "--pathspec-from-file paths.txt --pathspec-file-nul", "a.txt\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := claimFixture(t)
			claimEdit(t, repo, liveOwner, "A", "a.txt")
			hosttest.WriteRel(t, repo, "paths.txt", tc.content)
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "B", "agent_id": "leafB", "cwd": repo,
				"tool_name": "Bash", "tool_input": map[string]any{"command": "git commit -m x " + tc.option}}))
			if r.Decision != core.Ask || !strings.Contains(r.Reason, "a.txt takes") {
				t.Fatalf("%s: %v %q; want ask naming unstaged a.txt", tc.option, r.Decision, r.Reason)
			}
		})
	}
}

// TestStageOperandsCommitUntrackedIsNotAll: on commit -u is --untracked-files, so only -a/--all/-am stage all tracked.
func TestStageOperandsCommitUntrackedIsNotAll(t *testing.T) {
	cases := []struct {
		verb string
		args []string
		all  bool
	}{
		{"commit", []string{"-u", "-m", "x"}, false},
		{"commit", []string{"--update", "-m", "x"}, false},
		{"commit", []string{"-A", "-m", "x"}, false},
		{"commit", []string{"-a", "-m", "x"}, true},
		{"commit", []string{"--all", "-m", "x"}, true},
		{"commit", []string{"-am", "x"}, true},
		{"commit", []string{"-uall", "-m", "x"}, false},
		{"commit", []string{"-unormal", "-m", "x"}, false},
		{"commit", []string{"-uno", "-m", "x"}, false},
		{"commit", []string{"-sa", "-m", "x"}, true},
		{"add", []string{"-u"}, true},
	}
	for _, c := range cases {
		if all := splitStage(c.verb, c.args).stagesAll(c.verb); all != c.all {
			t.Errorf("splitStage(%s, %v) all=%v, want %v", c.verb, c.args, all, c.all)
		}
	}
}

// TestFirstSightingPersistsAcrossKeys: a key shows once, and a second distinct key never drops the first.
func TestFirstSightingPersistsAcrossKeys(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for i, step := range []struct {
		key  string
		want bool
	}{{"k1", true}, {"k1", false}, {"k2", true}, {"k1", false}, {"k2", false}} {
		if got := firstSighting("sess", step.key); got != step.want {
			t.Fatalf("step %d firstSighting(%q)=%v, want %v", i, step.key, got, step.want)
		}
	}
}

// TestClaimCheckNestedStaging: git add/commit the shell runs (nested, shell heredoc) asks; data heredoc text does not.
func TestClaimCheckNestedStaging(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	for _, c := range []struct {
		cmd string
		ask bool
	}{
		{"bash -c 'git add -A'", true},
		{"echo $(git commit -a -m x)", true},
		{"timeout -s KILL 5 git add -A", true},
		{"python3 - <<'EOF'\nprint('git add -A')\ngit add -A\nEOF", false},
		{"node <<'EOF'\ngit add -A\nEOF", false},
		{"cd . && python3 - <<'EOF'\ngit add -A\nEOF\necho done", false},
		{"cat > notes.txt <<'EOF'\ngit commit -am wip\nEOF", false},
		{"python3 - <<'EOF' && git add -A\nprint(1)\nEOF", true},
		{"python3 - <<EOF\nx = \"$(git add -A)\"\nEOF", true},
		{"python3 - <<'EOF' | sh\nprint('git add -A')\nEOF", true},
		{"cat > run.sh <<'EOF'\ngit add -A\nEOF\nbash run.sh", true},
		{"bash <<'EOF'\ngit add -A\nEOF", true},
		{"bash -s <<'EOF'\ngit add -A\nEOF", true},
		{"git add -A", true},
		{"git add a.txt", true},
		{"git add b.txt", false},
		{`git commit -m "mentions git add"`, false},
		{`echo "git add ."`, false},
		{"echo hi && git add -A", true},
		{"true && git commit -a -m x", true},
		{`echo "git add ." && echo ok`, false},
	} {
		r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": c.cmd})
		if want := map[bool]core.Decision{true: core.Ask, false: core.Allow}[c.ask]; r.Decision != want {
			t.Errorf("%q: %v %q; want %v", c.cmd, r.Decision, r.Reason, want)
		}
	}
}

func TestClaimCheckNestedTooDeepAsks(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "eval eval eval eval eval git add -A"})
	if r.Decision != core.Ask || !strings.Contains(r.Reason, "nests too deep") {
		t.Errorf("%v %q; want an ask naming the nesting", r.Decision, r.Reason)
	}
}

func TestClaimCheckLeafNestedCdIntoConfigBlocks(t *testing.T) {
	repo := claimFixture(t)
	other := paths.Real(hosttest.Repo(t))
	t.Setenv("CLAUDE_CONFIG_DIR", repo)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	for cmd, want := range map[string]core.Decision{
		"cd " + repo + " && eval eval eval eval eval git add -A": core.Block,
		"eval eval eval eval eval git add -A":                    core.Ask,
		"eval eval eval eval eval cd " + repo + " && git add -A": core.Block,
	} {
		r := ClaimCheck(claimInput(t, map[string]any{"session_id": "B", "agent_id": "leafone1", "cwd": other,
			"tool_name": "Bash", "tool_input": map[string]any{"command": cmd}}))
		if r.Decision != want {
			t.Errorf("%q: %v %q; want %v", cmd, r.Decision, r.Reason, want)
		}
	}
	wt := filepath.Join(paths.Real(t.TempDir()), "wt")
	hosttest.In(t, repo, "worktree", "add", "-q", "-b", "linked", wt)
	hosttest.WriteRel(t, wt, "sub/f.txt", "x\n")
	r := ClaimCheck(claimInput(t, map[string]any{"session_id": "B", "agent_id": "leafone1", "cwd": filepath.Join(wt, "sub"),
		"tool_name": "Bash", "tool_input": map[string]any{"command": "eval eval eval eval eval git add -A"}}))
	if r.Decision != core.Block {
		t.Errorf("subdirectory of a linked worktree: %v %q; want block", r.Decision, r.Reason)
	}
}

func TestClaimCheckTooDeepWithoutStageWordAllows(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "cat <<'EOF' > n.txt\ngit add x\nEOF\neval eval eval eval eval ls -la"})
	if r.Decision != core.Allow {
		t.Errorf("%v %q; want allow for a deep command whose only add is data", r.Decision, r.Reason)
	}
}

// TestFirstSightingLeavesNoLockFile: the seen file locks itself, so no .lock sibling is left behind.
func TestFirstSightingLeavesNoLockFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	firstSighting("sess", "k1")
	dir := filepath.Dir(seenFile("sess"))
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 || strings.HasSuffix(ents[0].Name(), ".lock") {
		t.Fatalf("claims-seen dir = %v (err %v); want only the seen file", ents, err)
	}
}

// TestFirstSightingFailsOpenOnHeldLock: a held lock costs at most seenWait, then the advisory still shows.
func TestFirstSightingFailsOpenOnHeldLock(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	file := seenFile("sess")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if !paths.TryLock(held) {
		t.Skip("file locking unsupported")
	}
	const wait = 50 * time.Millisecond
	t.Cleanup(core.SwapSeenWait(wait))
	start := time.Now()
	if !firstSighting("sess", "k1") {
		t.Fatal("held lock: want fail-open true")
	}
	if took := time.Since(start); took < wait {
		t.Fatalf("held lock: returned after %v, before the %v wait", took, wait)
	}
}

// TestClaimCheckExpiredBudgetFailsOpen: a check past its deadline allows with a warning instead of a verdict.
func TestClaimCheckExpiredBudgetFailsOpen(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	hookBudget(t, "worktree-claim-check", time.Nanosecond)
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add a.txt"})
	if r.Decision != core.Allow || !strings.Contains(r.Stderr, "could not check file claims") {
		t.Fatalf("expired budget: %v %q; want allow with the warning", r.Decision, r.Stderr)
	}
}

// TestStageOperandsAttachedValueTakesNoNextWord: git reads `-mc x` as message "c" with pathspec x.
func TestStageOperandsAttachedValueTakesNoNextWord(t *testing.T) {
	cases := []struct {
		args []string
		ops  []string
	}{
		{[]string{"-mc", "x"}, []string{"x"}},
		{[]string{"-um", "x"}, []string{"x"}},
		{[]string{"-aum", "x"}, []string{"x"}},
		{[]string{"-am", "msg", "y"}, []string{"y"}},
		{[]string{"-m", "msg", "y"}, []string{"y"}},
		{[]string{"--trailer", "Reviewed-by: x"}, nil},
		{[]string{"--cleanup", "strip", "y"}, []string{"y"}},
		{[]string{"--pathspec-from-file", "F", "y"}, []string{"y"}},
	}
	for _, c := range cases {
		if ops := splitStage("commit", c.args).ops; !slices.Equal(ops, c.ops) {
			t.Errorf("splitStage(commit, %v) ops=%v, want %v", c.args, ops, c.ops)
		}
	}
}

// TestClaimBashWritesAreClaimedAndChecked: a Bash sed -i is recorded by Claim and noted once by ClaimCheck.
func TestClaimBashWritesAreClaimedAndChecked(t *testing.T) {
	repo := claimFixture(t)
	useClaimOwner(t, liveOwner)
	hosttest.WriteRel(t, repo, "a.txt", "edited by A\n")
	Claim(claimInput(t, map[string]any{"session_id": "A", "cwd": repo, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "sed -i s/a/b/ a.txt && echo x > /dev/null"}}))
	got := readClaims(t, claimsFile(liveOwner, "A"))
	if len(got) != 1 || got[0].Path != filepath.Join(repo, "a.txt") {
		t.Fatalf("claims = %+v; want only a.txt", got)
	}
	bash := map[string]any{"command": "sed -i s/b/c/ a.txt"}
	if r := claimCheckOn(t, repo, "B", "Bash", bash); !strings.Contains(r.Context, "a.txt has uncommitted edits by") {
		t.Fatalf("sed -i of A's dirty file: context %q; want the advisory", r.Context)
	}
	if r := claimCheckOn(t, repo, "B", "Bash", bash); r.Context != "" {
		t.Fatalf("repeat must be silent, got %q", r.Context)
	}
}

// TestClaimSkipsPathItsCommandCommitted: a path the same command committed is not claimed; a still-dirty one is.
func TestClaimSkipsPathItsCommandCommitted(t *testing.T) {
	repo := claimFixture(t)
	useClaimOwner(t, liveOwner)
	hosttest.WriteRel(t, repo, "a.txt", "A's edit\n")
	hosttest.WriteRel(t, repo, "b.txt", "A's edit\n")
	hosttest.In(t, repo, "commit", "-qm", "A lands b", "--", "b.txt")
	Claim(claimInput(t, map[string]any{"session_id": "A", "cwd": repo, "tool_name": "Bash", "tool_input": map[string]any{
		"command": "printf x > a.txt && printf x > b.txt && git commit -qm x -- b.txt"}}))
	got := readClaims(t, claimsFile(liveOwner, "A"))
	if len(got) != 1 || got[0].Path != filepath.Join(repo, "a.txt") {
		t.Fatalf("claims = %+v; want only the still-dirty a.txt", got)
	}
}

// TestClaimCheckHintIsNonInteractive: the note and the ask name a recipe a hook shell can run, not `git add -p`.
func TestClaimCheckHintIsNonInteractive(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	note := claimCheckOn(t, repo, "B", "Edit", map[string]any{"file_path": filepath.Join(repo, "a.txt")}).Context
	ask := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add a.txt"})
	for name, text := range map[string]string{"note": note, "ask": ask.Reason} {
		if !strings.Contains(text, "git apply --cached <patch>") || strings.Contains(text, "git add -p") {
			t.Errorf("%s: %q; want the git apply --cached recipe and no git add -p", name, text)
		}
	}
	if ask.Decision != core.Ask {
		t.Fatalf("git add a.txt: %v; want ask", ask.Decision)
	}
}

// TestClaimCheckCleanRetiresOlderClaims: a claimed file seen clean drops claims made before; a later claim still asks.
func TestClaimCheckCleanRetiresOlderClaims(t *testing.T) {
	now := time.Now().Unix()
	t.Setenv("GIT_COMMITTER_DATE", time.Now().Add(-time.Hour).Format(time.RFC3339))
	repo := claimFixture(t)
	b := filepath.Join(repo, "b.txt")
	for i, tc := range []struct {
		name       string
		peerDirty  bool
		claimAfter bool
		want       core.Decision
	}{
		{"peer reverted before B's edit", false, false, core.Allow},
		{"peer still dirty", true, false, core.Ask},
		{"peer claims again after the clean", false, true, core.Ask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaimCase(t)
			session := "B" + strconv.Itoa(i) // clean marks live in the session's seen store
			hosttest.WriteRel(t, repo, "b.txt", "b\n")
			appendClaims(claimsFile(liveOwner, "A"), []worktree.Claim{{Path: b, TS: now - 60}})
			if tc.peerDirty {
				hosttest.WriteRel(t, repo, "b.txt", "A's edit\n")
			}
			claimCheckOn(t, repo, session, "Edit", map[string]any{"file_path": b})
			hosttest.WriteRel(t, repo, "b.txt", "B's edit\n")
			if tc.claimAfter {
				appendClaims(claimsFile(liveOwner, "A"), []worktree.Claim{{Path: b, TS: now + 60}})
			}
			if r := claimCheckOn(t, repo, session, "Bash", map[string]any{"command": "git add b.txt"}); r.Decision != tc.want {
				t.Fatalf("git add b.txt: %v %q; want %v", r.Decision, r.Reason, tc.want)
			}
		})
	}
}

// Regression (2026-11-16): edits of a clean claimed file add no clean mark once one postdates every claim.
func TestClaimCheckCleanMarkOncePerClaim(t *testing.T) {
	now := time.Now().Unix()
	repo := claimFixture(t)
	b := filepath.Join(repo, "b.txt")
	appendClaims(claimsFile(liveOwner, "A"), []worktree.Claim{{Path: b, TS: now - 60}})
	if _, err := core.SeenAdd(seenKind, "B", cleanMark+b+"\t"+strconv.FormatInt(now-30, 10)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		claimCheckOn(t, repo, "B", "Edit", map[string]any{"file_path": b})
	}
	if got, _ := os.ReadFile(seenFile("B")); strings.Count(string(got), cleanMark) != 1 {
		t.Fatalf("seen store:\n%s\nwant the one clean mark, no new line per edit", got)
	}
}

// TestClaimCheckInterruptedOrCutLeafEnded: an interrupted leaf or one whose last record is a token cut has ended.
func TestClaimCheckInterruptedOrCutLeafEnded(t *testing.T) {
	const (
		work = `{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash"}]}}`
		cut  = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,` +
			`"content":"[Request interrupted by user for tool use]"}]}}`
		cutText = `{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`
		denied  = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,` +
			`"content":"Permission to use Bash has been denied."}]}}`
		echoed = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1",` +
			`"content":[{"type":"text","text":"[Request interrupted by user]"}]}]}}`
		maxTok = `{"type":"assistant","message":{"stop_reason":"max_tokens","content":[{"type":"text","text":"x"}]}}`
		resume = `{"type":"user","isMeta":true,"message":{"content":"Output token limit hit. Resume directly"}}`
	)
	repo := claimFixture(t)
	for _, tc := range []struct {
		name  string
		lines []string
		want  core.Decision
	}{
		{"interrupted tool call", []string{work, cut}, core.Allow},
		{"interrupted turn", []string{work, cutText}, core.Allow},
		{"max_tokens last", []string{maxTok}, core.Allow},
		{"working", []string{work}, core.Ask},
		{"denied tool call", []string{work, denied}, core.Ask},
		{"marker in a tool's output", []string{work, echoed}, core.Ask},
		{"max_tokens being resumed", []string{maxTok, resume}, core.Ask},
		{"resumed after interrupt", []string{work, cut, work}, core.Ask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaimCase(t)
			useClaimOwner(t, liveOwner)
			tr := filepath.Join(t.TempDir(), "S.jsonl")
			hosttest.WriteRel(t, filepath.Dir(tr), "S/subagents/agent-leafone1.jsonl", strings.Join(tc.lines, "\n")+"\n")
			hosttest.WriteRel(t, repo, "a.txt", "leaf one\n")
			Claim(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leafone1", "cwd": repo,
				"transcript_path": tr, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}))
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "S", "cwd": repo, "transcript_path": tr,
				"tool_name": "Bash", "tool_input": map[string]any{"command": "git add a.txt"}}))
			if r.Decision != tc.want {
				t.Fatalf("main stages leaf's a.txt: %v %q; want %v", r.Decision, r.Reason, tc.want)
			}
		})
	}
}

// TestClaimCheckDryRunAndIntentStageNothing: a dry run or intent-to-add passes; staging or commit -n blocks.
func TestClaimCheckDryRunAndIntentStageNothing(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	for _, tc := range []struct {
		command string
		want    core.Decision
	}{
		{"git add -n a.txt", core.Allow},
		{"git add -An", core.Allow},
		{"git add --dry-run -A", core.Allow},
		{"git add -N a.txt", core.Allow},
		{"git add --intent-to-add -A", core.Allow},
		{"git commit --dry-run -a", core.Allow},
		{"git commit -n -am x", core.Ask},
		{"git add -n --no-dry-run a.txt", core.Ask},
		{"git add -- -n a.txt", core.Ask},
		{"git add -n a.txt && git add a.txt", core.Ask},
	} {
		t.Run(tc.command, func(t *testing.T) {
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "B", "agent_id": "leafB", "cwd": repo,
				"tool_name": "Bash", "tool_input": map[string]any{"command": tc.command}}))
			if r.Decision != tc.want {
				t.Fatalf("%s: %v %q; want %v", tc.command, r.Decision, r.Reason, tc.want)
			}
		})
	}
}

// The literal gates on gitStageCmd and core.GitCommitCmd change no verdict over every string in these tests.
func TestGitGatesMatchUngated(t *testing.T) {
	files, _ := filepath.Glob("*_test.go")
	more, _ := filepath.Glob("../docs/*_test.go")
	n := 0
	for _, f := range append(files, more...) {
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, g := range []*core.Gated{gitStageCmd, core.GitCommitCmd()} {
				if g.MatchString(s) != g.Regexp().MatchString(s) {
					t.Errorf("%s: gate changes the verdict on %q", g, s)
				}
			}
			n++
			return true
		})
	}
	if n < 100 {
		t.Fatalf("only %d test strings read, want the packages' corpus", n)
	}
}

// verbForms are one commit of a.txt written as the verb: plain, behind rtk, by absolute path, go run, in a chain.
var verbForms = []string{
	"claude-code worktree commit -m x a.txt",
	"rtk claude-code worktree commit -m x a.txt",
	"/usr/local/bin/claude-code worktree commit a.txt -m 'x y'",
	"cd . && go run ./cmd/claude-code worktree commit -m x a.txt && echo done",
}

// wrapperForms hold one commit inside a wrapper (%s): the verb form must be judged as the git form is.
var wrapperForms = []string{"bash -c '%s'", `sh -c "%s"`, "eval '%s'", "( %s )", "echo $(%s)", "echo `%s`", "env X=1 %s",
	"command %s", "true && %s", "true; %s", "false || %s", "echo hi | %s", "bash -c 'cd . && %s'", "bash -c \"sh -c '%s'\"",
	"sudo -u x %s", "nohup %s", "xargs %s", "watch '%s'", "time %s", "setsid sh -c '%s'", "bash <<< '%s'",
	"echo '%s' | sh", "bash -c 'bash -c \"%s\"'", "{ %s; }", "if true; then %s; fi"}

// withoutCommandLine drops the verdict's echo of the checked command, the one line that differs by spelling.
func withoutCommandLine(reason string) string {
	var keep []string
	for l := range strings.Lines(reason) {
		if !strings.HasPrefix(l, "  command:") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "")
}

// verbParity judges the verb form like the git form inside every wrapper and binary spelling.
func verbParity(t *testing.T, check func(string) core.Result) {
	t.Helper()
	for _, w := range wrapperForms {
		g := check(fmt.Sprintf(w, "git commit -m x -- a.txt"))
		v := check(fmt.Sprintf(w, "claude-code worktree commit -m x a.txt"))
		if v.Decision != g.Decision || withoutCommandLine(v.Reason) != withoutCommandLine(g.Reason) {
			t.Errorf("%q: verb %v %q; want the git verdict %v %q", w, v.Decision, v.Reason, g.Decision, g.Reason)
		}
	}
	for _, pair := range [][2]string{{`/usr/bin/git commit -m x -- a.txt`, `/usr/local/bin/claude-code worktree commit -m x a.txt`},
		{`"/usr/bin/git" commit -m x -- a.txt`, `"/usr/local/bin/claude-code" worktree commit -m x a.txt`},
		{`bash -c '"git" commit -m x -- a.txt'`, `bash -c '"claude-code" worktree commit -m x a.txt'`}} {
		g, v := check(pair[0]), check(pair[1])
		if v.Decision != g.Decision || withoutCommandLine(v.Reason) != withoutCommandLine(g.Reason) {
			t.Errorf("%q: verb %v %q; want the git verdict %v %q", pair[1], v.Decision, v.Reason, g.Decision, g.Reason)
		}
	}
}

// TestClaimCheckVerbCommitJudgedLikeGitCommit: the verb text gets the verdict of `git commit -- file`.
func TestClaimCheckVerbCommitJudgedLikeGitCommit(t *testing.T) {
	for _, tc := range []struct {
		name, claimSession, claimAgent, caller, agent string
		want                                          core.Decision
	}{
		{"peer session's file", "A", "", "B", "", core.Ask},
		{"sibling leaf's file, caller main", "S", "leafone1", "S", "", core.Ask},
		{"sibling leaf's file, caller leaf", "S", "leafone1", "S", "leaftwo2", core.Ask},
		{"main's file, caller leaf", "S", "", "S", "leaftwo2", core.Allow},
		{"own file, caller main", "S", "", "S", "", core.Allow},
		{"own file, caller leaf", "S", "leaftwo2", "S", "leaftwo2", core.Allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := claimFixture(t)
			tr := filepath.Join(t.TempDir(), "S.jsonl")
			for _, id := range []string{"leafone1", "leaftwo2"} {
				hosttest.WriteRel(t, filepath.Dir(tr), "S/subagents/agent-"+id+".jsonl", "{}\n")
			}
			useClaimOwner(t, liveOwner)
			hosttest.WriteRel(t, repo, "a.txt", "edited\n")
			claimIn := map[string]any{"session_id": tc.claimSession, "cwd": repo, "transcript_path": tr, "tool_name": "Edit",
				"tool_input": map[string]any{"file_path": "a.txt"}}
			if tc.claimAgent != "" {
				claimIn["agent_id"] = tc.claimAgent
			}
			Claim(claimInput(t, claimIn))
			useClaimOwner(t, checkerOwner)
			check := func(cmd string) core.Result {
				in := map[string]any{"session_id": tc.caller, "cwd": repo, "transcript_path": tr, "tool_name": "Bash",
					"tool_input": map[string]any{"command": cmd}}
				if tc.agent != "" {
					in["agent_id"] = tc.agent
				}
				return ClaimCheck(claimInput(t, in))
			}
			git := check("git commit -m x -- a.txt")
			if git.Decision != tc.want {
				t.Fatalf("git commit -- a.txt: %v %q; want %v", git.Decision, git.Reason, tc.want)
			}
			verbParity(t, check)
			for _, form := range verbForms {
				if r := check(form); r.Decision != git.Decision || withoutCommandLine(r.Reason) != withoutCommandLine(git.Reason) {
					t.Errorf("%q: %v %q; want the git verdict %v %q", form, r.Decision, r.Reason, git.Decision, git.Reason)
				}
			}
		})
	}
}

// TestClaimCheckVerbCommitWithoutFiles: the main session passes; a subagent is told to name its files.
func TestClaimCheckVerbCommitWithoutFiles(t *testing.T) {
	repo := claimFixture(t)
	for _, form := range []string{"claude-code worktree commit -m x", "rtk claude-code worktree commit -m 'x y'",
		"bash -c 'claude-code worktree commit -m x'", `claude-code worktree commit -m "--dry-run"`,
		"claude-code worktree commit -m 'x --dry-run'"} {
		main := claimCheckOn(t, repo, "S", "Bash", map[string]any{"command": form})
		if main.Decision != core.Allow {
			t.Fatalf("main %q: %v %q; want allow", form, main.Decision, main.Reason)
		}
		r := ClaimCheck(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leaftwo2", "cwd": repo,
			"tool_name": "Bash", "tool_input": map[string]any{"command": form}}))
		if r.Decision != core.Ask || !strings.Contains(r.Reason, "claude-code worktree commit -m <msg> <files>") {
			t.Fatalf("subagent %q: %v %q; want an ask to name its files", form, r.Decision, r.Reason)
		}
	}
}

// TestClaimCheckVerbTextIsNotACommit: the phrase as an argument is a search, never the verb, for a subagent too.
func TestClaimCheckVerbTextIsNotACommit(t *testing.T) {
	repo := claimFixture(t)
	for _, form := range []string{`rtk grep -rn "claude-code worktree commit" rules`, `grep -rn 'claude-code worktree commit' rules`,
		`rtk rg "claude-code worktree commit" .`, `echo claude-code worktree commit`, `git log --grep="claude-code worktree commit"`} {
		r := ClaimCheck(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leaftwo2", "cwd": repo,
			"tool_name": "Bash", "tool_input": map[string]any{"command": form}}))
		if r.Decision != core.Allow {
			t.Errorf("subagent %q: %v %q; want allow", form, r.Decision, r.Reason)
		}
	}
}

// TestClaimCheckVerbDryRunNamesNothing: a parsed --dry-run commits nothing, in every wrapper, for a subagent too.
func TestClaimCheckVerbDryRunNamesNothing(t *testing.T) {
	repo := claimFixture(t)
	for _, form := range []string{"claude-code worktree commit --dry-run", "claude-code worktree commit -m x --dry-run",
		"rtk claude-code worktree commit --dry-run -m 'x y'", "claude-code worktree commit -dry-run=true",
		"bash -c 'claude-code worktree commit --dry-run'", `sh -c "claude-code worktree commit -m x --dry-run"`,
		"eval 'claude-code worktree commit --dry-run'", "( claude-code worktree commit --dry-run )",
		"echo $(claude-code worktree commit --dry-run)", "env X=1 claude-code worktree commit --dry-run",
		"true && /usr/local/bin/claude-code worktree commit --dry-run a.txt"} {
		r := ClaimCheck(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leaftwo2", "cwd": repo,
			"tool_name": "Bash", "tool_input": map[string]any{"command": form}}))
		if r.Decision != core.Allow {
			t.Errorf("subagent %q: %v %q; want allow", form, r.Decision, r.Reason)
		}
	}
}

// TestClaimCheckHowToOncePerSession: only a session's first advisory carries the how-to.
func TestClaimCheckHowToOncePerSession(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	claimEdit(t, repo, liveOwner, "A", "b.txt")
	edit := func(session, rel string) string {
		return claimCheckOn(t, repo, session, "Edit", map[string]any{"file_path": filepath.Join(repo, rel)}).Context
	}
	first, second, other := edit("B", "a.txt"), edit("B", "b.txt"), edit("C", "a.txt")
	for _, c := range []struct{ name, got, file string }{{"first", first, "a.txt"}, {"second", second, "b.txt"}, {"new session", other, "a.txt"}} {
		if !strings.Contains(c.got, c.file+" has uncommitted edits by claude pid ") {
			t.Errorf("%s advisory %q lacks its file, holder or time", c.name, c.got)
		}
	}
	if !strings.Contains(first, howTo) || strings.Contains(second, "stage just those") || !strings.Contains(other, howTo) {
		t.Errorf("how-to placement wrong:\nfirst  %q\nsecond %q\nnew    %q", first, second, other)
	}
}

// TestClaimCheckJudgesEveryAgentLikeClaude: Codex, Copilot and OpenCode get Claude's verdict on one claim state.
func TestClaimCheckJudgesEveryAgentLikeClaude(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	patch := "*** Begin Patch\n*** Update File: a.txt\n@@\n-a\n+b\n*** End Patch\n"
	type shape struct {
		agent string
		doc   map[string]any
	}
	bash := func(cmd string) []shape {
		return []shape{
			{core.AgentClaude, map[string]any{"session_id": "B", "cwd": repo, "tool_name": "Bash", "tool_input": map[string]any{"command": cmd}}},
			{core.AgentCodex, map[string]any{"session_id": "B", "cwd": repo, "tool_name": "Bash", "tool_input": map[string]any{"command": cmd}}},
			{core.AgentCopilot, map[string]any{"sessionId": "B", "cwd": repo, "toolName": "bash", "toolArgs": map[string]any{"command": cmd}}},
			{core.AgentOpencode, map[string]any{"session_id": "B", "cwd": repo, "tool_name": "Bash", "harness_agent": "opencode", "tool_input": map[string]any{"command": cmd}}},
		}
	}
	edit := []shape{
		{core.AgentClaude, map[string]any{"session_id": "B", "cwd": repo, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}},
		{core.AgentCodex, map[string]any{"session_id": "B", "cwd": repo, "tool_name": "apply_patch", "tool_input": map[string]any{"command": patch}}},
		{core.AgentCopilot, map[string]any{"sessionId": "B", "cwd": repo, "toolName": "edit", "toolArgs": map[string]any{"path": "a.txt"}}},
		{core.AgentOpencode, map[string]any{"session_id": "B", "cwd": repo, "tool_name": "Edit", "harness_agent": "opencode", "tool_input": map[string]any{"file_path": "a.txt"}}},
	}
	for name, shapes := range map[string][]shape{"git add -A": bash("git add -A"), "edit a.txt": edit} {
		var want core.Result
		for i, s := range shapes {
			_ = os.Remove(seenFile("B")) // the advisory is once per session: each agent starts unseen
			useClaimOwner(t, checkerOwner)
			got := ClaimCheck(agentInput(t, s.agent, s.doc))
			if i == 0 {
				want = got
				if want.Decision != core.Ask && want.Context == "" {
					t.Fatalf("%s: claude's verdict %+v is neither an ask nor an advisory; the fixture proves nothing", name, want)
				}
				continue
			}
			// the reason's lead word differs where the agent has no ask (BLOCKED, not ASK); the files named do not
			if got.Decision != want.Decision || got.Context != want.Context || strings.Contains(want.Reason, "a.txt takes") != strings.Contains(got.Reason, "a.txt takes") {
				t.Errorf("%s as %s: %+v, want claude's %+v", name, s.agent, got, want)
			}
		}
	}
}

// TestClaimCheckLabelsPeerByItsCommandName: a live peer is named by its process's real command, not always claude.
func TestClaimCheckLabelsPeerByItsCommandName(t *testing.T) {
	comm := proc.Comm(os.Getpid())
	if comm == "" {
		t.Skip("no process name source on this platform")
	}
	repo := claimFixture(t)
	peer := os.Getpid()
	claimAlive = func(pid int) bool { return pid == peer }
	claimEdit(t, repo, peer, "A", "a.txt")
	got := claimCheckOn(t, repo, "B", "Edit", map[string]any{"file_path": filepath.Join(repo, "a.txt")}).Context
	if want := "a.txt has uncommitted edits by " + comm + " pid "; !strings.Contains(got, want) {
		t.Errorf("advisory %q lacks %q", got, want)
	}
}

// TestClaimCheckLanesMode: lanes mode refuses tracked edits and stages in the live checkout only; a lane stays open.
func TestClaimCheckLanesMode(t *testing.T) {
	for _, tc := range []struct {
		name, mode, file, cmd string
		lane                  bool
		agent                 string
		want                  core.Decision
	}{
		{"live checkout edit asks main", "lanes", "a.txt", "", false, "", core.Ask},
		{"live checkout edit blocks a leaf", "lanes", "a.txt", "", false, "leaftwo2", core.Block},
		{"live checkout stage asks main", "lanes", "a.txt", "git add a.txt", false, "", core.Ask},
		{"untracked file is free", "lanes", "new.txt", "", false, "", core.Allow},
		{"same path in a lane is free", "lanes", "a.txt", "", true, "leaftwo2", core.Allow},
		{"stage in a lane is free", "lanes", "a.txt", "git add a.txt", true, "", core.Allow},
		{"not lanes mode", "", "a.txt", "", false, "leaftwo2", core.Allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := claimFixture(t)
			t.Setenv("CLAUDE_CONFIG_DIR", repo)
			t.Setenv(worktreeModeEnv, tc.mode)
			root := repo
			if tc.lane {
				root = filepath.Join(paths.Real(t.TempDir()), "lane")
				hosttest.In(t, repo, "worktree", "add", "-q", "-b", "lane", root)
			}
			hosttest.WriteRel(t, root, tc.file, "edited\n")
			in := map[string]any{"session_id": "S", "cwd": root, "agent_id": tc.agent, "tool_name": "Edit",
				"tool_input": map[string]any{"file_path": filepath.Join(root, tc.file)}}
			if tc.cmd != "" {
				in["tool_name"], in["tool_input"] = "Bash", map[string]any{"command": tc.cmd}
			}
			useClaimOwner(t, checkerOwner)
			r := ClaimCheck(claimInput(t, in))
			if r.Decision != tc.want {
				t.Fatalf("%v %q; want %v", r.Decision, r.Reason, tc.want)
			}
			if tc.want != core.Allow && !strings.Contains(r.Reason, "claude-code worktree create <slug>") {
				t.Errorf("reason %q must name the lane command", r.Reason)
			}
		})
	}
}
