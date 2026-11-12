package tools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/paths"
)

// Link points BinDir/<bin> at each agent's PATH copy (names empty = all); never fatal, errors print.
func Link(w io.Writer, names []string, dryRun bool) {
	for _, r := range LinkResults(names, dryRun) {
		fmt.Fprintf(w, "  %-8s %s\n", r.Status, r.Detail)
	}
}

// LinkResult is one agent's bin/ link outcome: Status is the %-8s verb Link prints
// ("ok", "link", "keep", "skip", "remove", "missing", "error"); Bin is the BinDir/<bin> name.
type LinkResult struct {
	Bin, Status, Detail string
}

// LinkResults computes Link's per-agent outcomes (applying the side effects on a real run),
// for callers that render a compact summary instead of one line per agent.
func LinkResults(names []string, dryRun bool) []LinkResult {
	var out []LinkResult
	for _, t := range tools {
		bin, ok := paths.AgentName(t.Name)
		if !ok || (len(names) > 0 && !slices.Contains(names, t.Name)) {
			continue
		}
		status, detail := linkOne(t, bin, dryRun)
		out = append(out, LinkResult{Bin: bin, Status: status, Detail: detail})
	}
	return out
}

// linkOne applies the bin/ rule to one agent: keep a regular file, else link the PATH entry, else
// drop the link; returns the %-8s status verb and its detail text.
func linkOne(t tool, bin string, dryRun bool) (status, detail string) {
	dst := filepath.Join(paths.BinDir(), bin)
	c := linkCandidate(t)
	missing := fmt.Sprintf("%s not installed (claude-code install --tools=%s)", bin, t.Name)
	fi, err := os.Lstat(dst)
	switch {
	case err == nil:
		return linkExisting(t, dst, c, missing, fi, dryRun)
	case !os.IsNotExist(err):
		return "error", err.Error()
	case c != "":
		return linkNew(dst, c, dryRun)
	}
	return "missing", missing
}

// linkExisting classifies an existing dst: keep a regular file or a matching link, else relink or drop it.
func linkExisting(t tool, dst, c, missing string, fi os.FileInfo, dryRun bool) (status, detail string) {
	isLink := fi.Mode()&os.ModeSymlink != 0
	switch {
	case fi.Mode().IsRegular():
		line := dst + " (installed here)"
		if c != "" {
			line += " · also on PATH: " + c
		}
		return "keep", line
	case !isLink:
		return "skip", dst + " (not a file or link)"
	case c != "" && readlink(dst) == c:
		return "ok", dst + " → " + c
	case c != "":
		return linkNew(dst, c, dryRun)
	}
	return dropLink(t, dst, missing, dryRun)
}

// linkNew links dst to the PATH candidate c (dry run: reports it only).
func linkNew(dst, c string, dryRun bool) (status, detail string) {
	suffix := ""
	if npmPrefixFromBinary(c) != "" {
		suffix = " (npm-global)"
	}
	if dryRun {
		return "link", dst + " → " + c + suffix
	}
	switch kept, err := replaceLink(dst, c); {
	case err != nil:
		return "error", err.Error()
	case kept:
		// dst became a regular file between the check above and the rename: keep it, not the link.
		return "keep", dst + " (installed here)"
	default:
		return "link", dst + " → " + c + suffix
	}
}

// dropLink handles a link with no PATH/local-bin candidate.
func dropLink(t tool, dst, missing string, dryRun bool) (status, detail string) {
	// A symlink to a real, executable, non-shim target is the user's own working link — keep it;
	// only a dangling or shim-pointing link is removed.
	if _, err := paths.AgentBin(t.Name); err == nil {
		return "keep", dst + " → " + readlink(dst) + " (existing link)"
	}
	if dryRun {
		return "remove", dst + "; " + missing
	}
	switch kept, err := removeUnlessFile(dst); {
	case err != nil:
		return "error", err.Error()
	case kept:
		return "keep", dst + " (installed here)"
	}
	return "remove", dst + "; " + missing
}

// removeUnlessFile removes dst under the bin lock; kept=true when a direct install now occupies dst.
func removeUnlessFile(dst string) (kept bool, err error) {
	err = withBinLock(filepath.Dir(dst), func() error {
		if fi, lerr := os.Lstat(dst); lerr == nil && fi.Mode().IsRegular() {
			kept = true
			return nil
		}
		return os.Remove(dst)
	})
	return kept, err
}

// linkCandidate is the agent's PATH entry made absolute (unresolved, so self-updaters are followed), else ~/.local/bin, else "".
func linkCandidate(t tool) string {
	if c, err := paths.LookPathReal(t.Binary); err == nil {
		abs, aerr := filepath.Abs(c) // a relative PATH entry would make the symlink target resolve from bin/
		if aerr != nil {
			return ""
		}
		return abs
	}
	local := localBinDir()
	if local == "" {
		return ""
	}
	c := filepath.Join(local, t.Binary)
	if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
		return c
	}
	return ""
}

func readlink(p string) string {
	target, _ := os.Readlink(p)
	return target
}

// replaceLink atomically links dst to target under the bin lock; kept=true leaves a regular file (direct install).
func replaceLink(dst, target string) (kept bool, err error) {
	err = withBinLock(filepath.Dir(dst), func() error {
		switch fi, lerr := os.Lstat(dst); {
		case lerr == nil && fi.Mode().IsRegular():
			kept = true
			return nil
		case lerr != nil && !os.IsNotExist(lerr):
			return lerr
		}
		return paths.SwapSymlink(target, dst)
	})
	return kept, err
}
