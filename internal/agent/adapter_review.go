package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/gitexec"
)

// Scope is the diff range `<v> review` acts on. Kind: uncommitted|base|commit|pr; Ref holds the
// base branch (base), the sha (commit), or is empty (uncommitted, pr).
type Scope struct{ Kind, Ref string }

// ParseScope validates that at most one scope flag was set; none selects uncommitted.
func ParseScope(uncommitted bool, base, commit string, pr bool) (Scope, error) {
	for _, ref := range []string{base, commit} {
		if strings.HasPrefix(ref, "-") {
			return Scope{}, fmt.Errorf("review: --base/--commit ref %q must not start with -", ref)
		}
	}
	n := 0
	for _, set := range []bool{uncommitted, base != "", commit != "", pr} {
		if set {
			n++
		}
	}
	if n > 1 {
		return Scope{}, errors.New("review: at most one of --uncommitted, --base, --commit, --pr")
	}
	switch {
	case base != "":
		return Scope{Kind: "base", Ref: base}, nil
	case commit != "":
		return Scope{Kind: "commit", Ref: commit}, nil
	case pr:
		return Scope{Kind: "pr"}, nil
	default:
		return Scope{Kind: "uncommitted"}, nil
	}
}

// untrackedCap caps how much of one untracked file's content Diff inlines.
const untrackedCap = 64 * 1024

// untrackedTotalCap caps all untracked content Diff inlines together.
const untrackedTotalCap = 512 * 1024

// binarySniff is how many leading bytes are checked for a NUL (binary) marker.
const binarySniff = 8 * 1024

// Diff materializes s's diff in cwd via gitexec. Empty content is an error, not an empty slice.
func Diff(ctx context.Context, cwd string, s Scope) ([]byte, error) {
	var out string
	var err error
	switch s.Kind {
	case "base":
		out, err = gitexec.RunGit(ctx, cwd, "diff", "--end-of-options", s.Ref+"...HEAD")
	case "commit":
		out, err = gitexec.RunGit(ctx, cwd, "show", "--no-color", "--format=", "--end-of-options", s.Ref)
	case "pr":
		var base string
		base, err = gitexec.RunGit(ctx, cwd, "rev-parse", "--abbrev-ref", "origin/HEAD")
		if err != nil {
			return nil, err
		}
		out, err = gitexec.RunGit(ctx, cwd, "diff", "--end-of-options", strings.TrimSpace(base)+"...HEAD") // origin/<default>, not local
	default: // uncommitted
		out, err = gitexec.RunGit(ctx, cwd, "diff", "HEAD")
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

// untrackedBlock renders every untracked regular text file as a "new file: <path>" header plus content,
// capped at untrackedCap per file and untrackedTotalCap overall; non-regular and binary files get a marker.
func untrackedBlock(ctx context.Context, cwd string) (string, error) {
	listing, err := gitexec.RunGit(ctx, cwd, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	total, skipped := 0, 0
	for rel := range strings.SplitSeq(listing, "\n") {
		if rel == "" {
			continue
		}
		if total >= untrackedTotalCap {
			skipped++
			continue
		}
		path := filepath.Join(cwd, rel)
		info, lerr := os.Lstat(path)
		if lerr != nil {
			continue // vanished between listing and read
		}
		if !info.Mode().IsRegular() {
			kind := "special file"
			if info.Mode()&os.ModeSymlink != 0 {
				kind = "symlink"
			}
			fmt.Fprintf(&b, "new file: %s (%s, skipped)\n", rel, kind)
			continue
		}
		data, rerr := readCapped(path)
		if rerr != nil {
			continue // skip rather than fail the whole diff
		}
		if bytes.IndexByte(data[:min(len(data), binarySniff)], 0) >= 0 {
			fmt.Fprintf(&b, "new file: %s (binary, skipped)\n", rel)
			continue
		}
		total += len(data)
		fmt.Fprintf(&b, "new file: %s\n%s\n", rel, data)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "… %d more untracked files not inlined\n", skipped)
	}
	return b.String(), nil
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

// reviewPromptAdversarial is agents/codex.md's "Adversarial diff review (`/codex`)" prompt, verbatim.
const reviewPromptAdversarial = `You are an adversarial reviewer: find what the author missed, not what is correct. Review the attached diff in
priority order — CRITICAL security, data loss, races, leaks; HIGH bugs, broken error handling, API misuse;
MEDIUM missing tests for new behavior, brittle assumptions, unhandled edge cases; LOW maintainability.
Skepticism ON: name the hidden assumption each change depends on. No praise, no summary — findings only, one
per line: <severity> <file:line> <issue> <fix>. If the diff is clean, say exactly: VERDICT: no actionable findings.`

// ReviewPrompt returns the base review prompt, with a "Focus: " line appended when focus is set.
func ReviewPrompt(adversarial bool, focus string) string {
	p := reviewPromptNeutral
	if adversarial {
		p = reviewPromptAdversarial
	}
	if focus != "" {
		p += "\nFocus: " + focus
	}
	return p
}

// ReviewArgs builds the `ask` argv for a review: the diff lands in state/harness/dispatch/review-*.diff attached
// via --attach. cleanup removes it in foreground mode; in background mode the detached child reads it and
// `claude-code clean` (dispatch-log target) sweeps it. rubric, when set, emits --rubric <f> before the prompt.
func ReviewArgs(agentName string, s Scope, diff []byte, adversarial, background bool, focus, rubric string) ([]string, func(), error) {
	dir := DispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	args := []string{"--agent", agentName, "--fallback", FallbackAgents, "--attach", f.Name()}
	if background {
		args = append(args, "--background")
	}
	if rubric != "" {
		args = append(args, "--rubric", rubric)
	}
	args = append(args, ReviewPrompt(adversarial, focus))
	cleanup := func() {}
	if !background {
		cleanup = func() { _ = os.Remove(f.Name()) } // best-effort cleanup of the review diff temp file
	}
	return args, cleanup, nil
}
