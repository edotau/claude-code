package hooks

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
)

var (
	gitCommitCmd = regexp.MustCompile(`\bgit\b[^;&|\n]*\bcommit\b`)
	commitLine   = regexp.MustCompile(`(?m)^\[(?:detached HEAD|[^\]\s]+)(?: \(root-commit\))? ([0-9a-f]{7,40})\] `)
	gitDirArg    = regexp.MustCompile(`\bgit\s+(?:-\S+\s+)*?-C\s+("[^"]+"|'[^']+'|\S+)`)
	cmdSegment   = regexp.MustCompile(`&&|\|\||[;|\n]`)
	leadingCD    = regexp.MustCompile(`^\s*cd\s+("[^"]+"|'[^']+'|[^\s&;]+)\s*&&`)
)

// PlanTick is the PostToolUse(Bash) plan-tick hook: a landed `git commit` ticks the active plan's covered tasks.
func PlanTick(r io.Reader, stdout, stderr io.Writer) int {
	in := ParseInput(r)
	cmd := in.ToolCommand()
	if in.ToolName != "Bash" || !gitCommitCmd.MatchString(cmd) {
		return ExitProceed
	}
	out := in.nested("tool_response", "stdout")
	if out == "" {
		out = in.ToolResponseText()
	}
	var blocks []string
	seen := map[string]bool{}
	for _, m := range commitLine.FindAllStringSubmatch(out, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			blocks = append(blocks, tickCommit(in, cmd, m[1], stderr)...)
		}
	}
	fmt.Fprint(stdout, MergeContext("PostToolUse", blocks, in.Vendor()))
	return ExitProceed
}

// tickCommit ticks the active plan's tasks covered by one landed commit and returns the context blocks.
func tickCommit(in *Input, cmd, sha string, stderr io.Writer) []string {
	ctx, cancel := hookContext("docs-plan-tick")
	defer cancel()
	dir := commitRepoDir(ctx, in.CWD, cmd, sha)
	if dir == "" {
		return nil
	}
	top := memory.GitToplevel(dir)
	worktree := "" // a linked worktree's name, matched against task claims
	if main := paths.MainCheckout(top); main != top {
		worktree = filepath.Base(top)
	}
	plan, ok := plans.Active(plans.Home(top), worktree, mirrorDests())
	if !ok {
		return nil
	}
	raw, err := exec.CommandContext(ctx, "git", "-C", dir, "show", "-z", "--name-only", "--format=", sha).Output()
	if err != nil {
		fmt.Fprintf(stderr, "plan-tick: %v\n", err)
		return nil
	}
	var files []string
	for _, f := range strings.Split(string(raw), "\x00") {
		if f = strings.TrimPrefix(f, "\n"); f != "" {
			files = append(files, f)
		}
	}
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	ticked, partial, err := plans.Tick(plan, short, files)
	if err != nil {
		fmt.Fprintf(stderr, "plan-tick: %v\n", err)
		return nil
	}
	base := strings.TrimSuffix(filepath.Base(plan), ".md")
	if len(partial) > 0 {
		recordPartials(in.SessionID, base, plan, partial, stderr)
	}
	var blocks []string
	if len(ticked) > 0 {
		blocks = append(blocks, "plans: ticked "+strings.Join(ticked, "; ")+" ("+short+") in "+base)
	}
	if len(partial) > 0 {
		blocks = append(blocks, "plans: partly landed "+strings.Join(partial, "; ")+" in "+base)
	}
	return blocks
}

// commitRepoDir is the candidate dir (commit -C args, leading cd, cwd) whose HEAD is sha, else the first whose repo holds it.
func commitRepoDir(ctx context.Context, cwd, cmd, sha string) string {
	var cands []string
	for _, seg := range cmdSegment.Split(cmd, -1) { // only commit invocations name a candidate repo
		if !gitCommitCmd.MatchString(seg) {
			continue
		}
		if m := gitDirArg.FindStringSubmatch(seg); m != nil {
			cands = append(cands, strings.Trim(m[1], `"'`))
		}
	}
	if m := leadingCD.FindStringSubmatch(cmd); m != nil {
		cands = append(cands, strings.Trim(m[1], `"'`))
	}
	cands = append(cands, cwd)
	home, _ := os.UserHomeDir()
	var holders []string
	for _, d := range cands {
		if rest, ok := strings.CutPrefix(d, "~/"); ok && home != "" {
			d = filepath.Join(home, rest) // the shell expands ~ before git sees it
		}
		if d = resolveCWDPath(d, cwd); d == "" {
			continue
		}
		if exec.CommandContext(ctx, "git", "-C", d, "cat-file", "-e", sha+"^{commit}").Run() != nil {
			continue
		}
		head, err := exec.CommandContext(ctx, "git", "-C", d, "rev-parse", "HEAD").Output()
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(head)), sha) {
			return d // linked worktrees share objects; the one whose HEAD is the commit made it
		}
		holders = append(holders, d)
	}
	if len(holders) > 0 {
		return holders[0]
	}
	return ""
}

// recordPartials appends "<path>\\t<plan>: <task>" lines for the Stop reminder.
func recordPartials(session, plan, planPath string, tasks []string, stderr io.Writer) {
	if err := os.MkdirAll(plansScratchDir(), 0o700); err != nil {
		fmt.Fprintf(stderr, "plan-tick: %v\n", err)
		return
	}
	f, err := os.OpenFile(partialFile(session), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "plan-tick: %v\n", err)
		return
	}
	defer f.Close()
	for _, t := range tasks {
		fmt.Fprintf(f, "%s\t%s: %s\n", planPath, plan, t)
	}
}
