package ask

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/pack"

	"github.com/edotau/claude-code/internal/paths"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
)

// diffScope is the diff range `<v> review` acts on. Kind: uncommitted|base|commit|pr; Ref holds the
// base branch (base), the sha (commit), or is empty (uncommitted, pr).
type diffScope struct{ Kind, Ref string }

// parseScope validates that at most one scope flag was set; none selects uncommitted.
func parseScope(uncommitted bool, base, commit string, pr bool) (diffScope, error) {
	for _, ref := range []string{base, commit} {
		if strings.HasPrefix(ref, "-") {
			return diffScope{}, fmt.Errorf("review: --base/--commit ref %q must not start with -", ref)
		}
	}
	n := 0
	for _, set := range []bool{uncommitted, base != "", commit != "", pr} {
		if set {
			n++
		}
	}
	if n > 1 {
		return diffScope{}, errors.New("review: at most one of --uncommitted, --base, --commit, --pr")
	}
	switch {
	case base != "":
		return diffScope{Kind: "base", Ref: base}, nil
	case commit != "":
		return diffScope{Kind: "commit", Ref: commit}, nil
	case pr:
		return diffScope{Kind: "pr"}, nil
	default:
		return diffScope{Kind: "uncommitted"}, nil
	}
}

// rangeDiff is `git diff <ref>...HEAD`.
func rangeDiff(ctx context.Context, cwd, ref string) (string, error) {
	return proc.RunGit(ctx, cwd, "diff", "--end-of-options", ref+"...HEAD")
}

// untrackedCap caps how much of one untracked file's content Diff inlines.
const untrackedCap = 64 * 1024

// untrackedTotalCap caps all untracked content Diff inlines together.
const untrackedTotalCap = 512 * 1024

// binarySniff is how many leading bytes are checked for a NUL (binary) marker.
const binarySniff = 8 * 1024

// diff materializes s's diff in cwd via proc. Empty content is an error, not an empty slice.
func diff(ctx context.Context, cwd string, s diffScope) ([]byte, error) {
	var out string
	var err error
	switch s.Kind {
	case "base":
		out, err = rangeDiff(ctx, cwd, s.Ref)
	case "commit":
		// first-parent: a merge's default combined diff is empty for a clean merge
		out, err = proc.RunGit(ctx, cwd, "show", "--no-color", "--format=", "--diff-merges=first-parent", "--end-of-options", s.Ref)
	case "pr":
		var base string
		base, err = proc.RunGit(ctx, cwd, "rev-parse", "--abbrev-ref", "origin/HEAD")
		if err != nil {
			return nil, err
		}
		out, err = rangeDiff(ctx, cwd, strings.TrimSpace(base)) // origin/<default>, not local
	default: // uncommitted
		out, err = proc.RunGit(ctx, cwd, "diff", "HEAD")
		if err == nil {
			var untracked string
			untracked, err = untrackedBlock(ctx, cwd)
			if err == nil && untracked != "" {
				out = strings.TrimRight(out, "\n") + "\n" + untracked
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, errors.New("nothing in scope")
	}
	return []byte(out), nil
}

// untrackedBlock renders untracked text files as "new file: <path>" plus content, capped per file and overall.
func untrackedBlock(ctx context.Context, cwd string) (string, error) {
	listing, err := proc.RunGit(ctx, cwd, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	total, skipped := 0, 0
	for rel := range strings.SplitSeq(listing, "\x00") { // -z: names are raw, never C-quoted
		if rel == "" {
			continue
		}
		if total >= untrackedTotalCap {
			skipped++
			continue
		}
		total += appendUntrackedWithin(&b, cwd, rel, untrackedTotalCap-total)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "… %d more untracked files not inlined\n", skipped)
	}
	return b.String(), nil
}

// appendUntrackedWithin renders one untracked file, inlining at most limit bytes of its content.
func appendUntrackedWithin(b *strings.Builder, cwd, rel string, limit int) int {
	path := filepath.Join(cwd, rel)
	info, lerr := os.Lstat(path)
	if lerr != nil {
		fmt.Fprintf(b, "new file: %s (unreadable, skipped: %v)\n", rel, lerr)
		return 0
	}
	if !info.Mode().IsRegular() {
		b.WriteString(skippedSpecialNote(rel, info))
		return 0
	}
	if pack.SecretLike(rel) {
		fmt.Fprintf(b, "new file: %s (credential-like name, not inlined)\n", rel)
		return 0
	}
	data, rerr := readCapped(path)
	if rerr != nil {
		fmt.Fprintf(b, "new file: %s (unreadable, skipped: %v)\n", rel, rerr)
		return 0
	}
	if bytes.IndexByte(data[:min(len(data), binarySniff)], 0) >= 0 {
		fmt.Fprintf(b, "new file: %s (binary, skipped)\n", rel)
		return 0
	}
	data = data[:min(len(data), limit)] // keeps the running total within untrackedTotalCap
	fmt.Fprintf(b, "new file: %s\n%s\n", rel, data)
	if info.Size() > int64(len(data)) {
		fmt.Fprintf(b, "(truncated at %d bytes)\n", len(data))
	}
	return len(data)
}

// skippedSpecialNote is the "new file" line for a symlink or other non-regular untracked file.
func skippedSpecialNote(rel string, info fs.FileInfo) string {
	kind := "special file"
	if info.Mode()&os.ModeSymlink != 0 {
		kind = "symlink"
	}
	return fmt.Sprintf("new file: %s (%s, skipped)\n", rel, kind)
}

// readCapped reads at most untrackedCap bytes of path without loading a larger file whole.
func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, untrackedCap+1))
	if err != nil {
		return nil, err
	}
	if len(data) > untrackedCap {
		data = data[:untrackedCap]
	}
	return data, nil
}

// reviewPromptNeutral mirrors the adversarial prompt's shape with a non-adversarial brief.
const reviewPromptNeutral = `Review the attached diff for correctness, regressions, missing tests, and scope creep.
Findings as one per line: [SEVERITY] file:line — issue — fix, ordered CRITICAL to LOW.
The first line of your answer is the verdict alone: SHIP IT, NEEDS WORK or BLOCK.`

// reviewPromptAdversarial asks for severity-ordered findings only, with a fixed clean-diff verdict.
const reviewPromptAdversarial = `You are an adversarial reviewer: find what the author missed, not what is correct. Review the attached diff in
priority order — CRITICAL security, data loss, races, leaks; HIGH bugs, broken error handling, API misuse;
MEDIUM missing tests for new behavior, brittle assumptions, unhandled edge cases; LOW maintainability.
Skepticism ON: name the hidden assumption each change depends on. No praise, no summary — findings only, one
per line: <severity> <file:line> <issue> <fix>. If the diff is clean, say exactly: ` + cleanVerdict + `.`

// reviewPrompt returns the base review prompt, with a "Focus: " line appended when focus is set.
func reviewPrompt(adversarial bool, focus string) string {
	p := reviewPromptNeutral
	if adversarial {
		p = reviewPromptAdversarial
	}
	if focus != "" {
		p += "\nFocus: " + focus
	}
	return p
}

// reviewArgs builds the `ask` argv for a review, attaching the diff as a temp file; cleanup removes it in foreground.
func reviewArgs(agentName string, diff []byte, adversarial, background bool, focus, rubric, model string) ([]string, func(), error) {
	dir, err := paths.EnsureDispatchDir()
	if err != nil {
		return nil, nil, err
	}
	f, err := os.CreateTemp(dir, "review-*.diff")
	if err != nil {
		return nil, nil, err
	}
	if _, err := f.Write(diff); err != nil {
		_ = f.Close()           // best-effort cleanup; the write error is already being returned
		_ = os.Remove(f.Name()) // best-effort cleanup; the write error is already being returned
		return nil, nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name()) // best-effort cleanup; the close error is already being returned
		return nil, nil, err
	}
	args := []string{"--agent", agentName, "--fallback", router.FallbackAgents, "--attach", f.Name()}
	if background {
		args = append(args, "--background")
	}
	if rubric != "" {
		args = append(args, "--rubric", rubric)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, reviewPrompt(adversarial, focus))
	cleanup := func() {}
	if !background {
		cleanup = func() { _ = os.Remove(f.Name()) } // best-effort cleanup of the review diff temp file
	}
	return args, cleanup, nil
}

// reviewParts reports the parts an oversized diff splits into (one part means the plain single call).
func reviewParts(diff []byte, adversarial bool, focus, rubric string) []part {
	promptLen := len(reviewPrompt(adversarial, focus))
	if fi, err := os.Stat(rubric); rubric != "" && err == nil {
		promptLen += int(fi.Size())
	}
	return splitDiff(diff, promptLen, reviewBudget)
}

// setReviewProgress stores p on the job record, when there is one.
func setReviewProgress(job *agent.Record, p string) {
	if job == nil {
		return
	}
	job.Progress = p
	_ = job.Save()
}

// lastDispatchError is the newest failed dispatch's error text for agent, so a split report names the real cause.
func lastDispatchError(name string) string {
	for _, r := range agent.RecentRecords(agent.AllRecords) { // every record: other agents' newer jobs must not hide this leg's failure
		if r.Agent == name && r.Status == "error" {
			return textutil.PreviewRedacted(r.Error, 400)
		}
	}
	return "see claude-code " + name + " status"
}
