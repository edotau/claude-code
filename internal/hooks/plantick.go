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
	"github.com/edotau/claude-code/internal/plans"
)

var (
	gitCommitCmd = regexp.MustCompile(`\bgit\b[^;&|\n]*\bcommit\b`)
	commitLine   = regexp.MustCompile(`(?m)^\[[^\]\s]+(?: \(root-commit\))? ([0-9a-f]{7,40})\] `)
	gitDirArg    = regexp.MustCompile(`\bgit\s+(?:-\S+\s+)*?-C\s+("[^"]+"|'[^']+'|\S+)`)
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
	m := commitLine.FindStringSubmatch(out)
	if m == nil {
		return ExitProceed
	}
	sha := m[1]
	ctx, cancel := hookContext("plan-tick")
	defer cancel()
	dir := commitRepoDir(ctx, in.CWD, cmd, sha)
	if dir == "" {
		return ExitProceed
	}
	plan, ok := plans.Active(plans.Home(memory.GitToplevel(dir)))
	if !ok {
		return ExitProceed
	}
	files, err := exec.CommandContext(ctx, "git", "-C", dir, "show", "--name-only", "--format=", sha).Output()
	if err != nil {
		fmt.Fprintf(stderr, "plan-tick: %v\n", err)
		return ExitProceed
	}
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	ticked, partial, err := plans.Tick(plan, short, strings.Split(strings.TrimSpace(string(files)), "\n"))
	if err != nil {
		fmt.Fprintf(stderr, "plan-tick: %v\n", err)
		return ExitProceed
	}
	base := strings.TrimSuffix(filepath.Base(plan), ".md")
	if len(partial) > 0 {
		recordPartials(in.SessionID, base, partial, stderr)
	}
	var blocks []string
	if len(ticked) > 0 {
		blocks = append(blocks, "plans: ticked "+strings.Join(ticked, "; ")+" ("+short+") in "+base)
	}
	if len(partial) > 0 {
		blocks = append(blocks, "plans: partly landed "+strings.Join(partial, "; ")+" in "+base)
	}
	fmt.Fprint(stdout, MergeContext("PostToolUse", blocks, in.Vendor()))
	return ExitProceed
}

// commitRepoDir is the first candidate dir (-C arg, leading cd, cwd) whose repo holds the commit sha.
func commitRepoDir(ctx context.Context, cwd, cmd, sha string) string {
	var cands []string
	if m := gitDirArg.FindStringSubmatch(cmd); m != nil {
		cands = append(cands, strings.Trim(m[1], `"'`))
	}
	if m := leadingCD.FindStringSubmatch(cmd); m != nil {
		cands = append(cands, strings.Trim(m[1], `"'`))
	}
	cands = append(cands, cwd)
	home, _ := os.UserHomeDir()
	for _, d := range cands {
		if rest, ok := strings.CutPrefix(d, "~/"); ok && home != "" {
			d = filepath.Join(home, rest) // the shell expands ~ before git sees it
		}
		if d = resolveCWDPath(d, cwd); d == "" {
			continue
		}
		if exec.CommandContext(ctx, "git", "-C", d, "cat-file", "-e", sha+"^{commit}").Run() == nil {
			return d
		}
	}
	return ""
}

// recordPartials appends "<plan>: <task>" lines for the Stop reminder.
func recordPartials(session, plan string, tasks []string, stderr io.Writer) {
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
		fmt.Fprintf(f, "%s: %s\n", plan, t)
	}
}
