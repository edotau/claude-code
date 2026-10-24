package tools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/paths"
)

// LinkVendors points BinDir/<bin> at each vendor's PATH copy (names empty = all); never fatal, errors print.
func LinkVendors(w io.Writer, names []string, dryRun bool) {
	for _, r := range LinkResults(names, dryRun) {
		fmt.Fprintf(w, "  %-8s %s\n", r.Status, r.Detail)
	}
}

// LinkResult is one vendor's bin/ link outcome: Status is the %-8s verb LinkVendors prints
// ("ok", "link", "keep", "skip", "remove", "missing", "error"); Bin is the BinDir/<bin> name.
type LinkResult struct {
	Bin, Status, Detail string
}

// LinkResults computes LinkVendors' per-vendor outcomes (applying the side effects on a real run),
// for callers that render a compact summary instead of one line per vendor.
func LinkResults(names []string, dryRun bool) []LinkResult {
	var out []LinkResult
	for _, t := range Tools {
		bin, ok := paths.VendorName(t.Name)
		if !ok || (len(names) > 0 && !slices.Contains(names, t.Name)) {
			continue
		}
		status, detail := linkOne(t, bin, dryRun)
		out = append(out, LinkResult{Bin: bin, Status: status, Detail: detail})
	}
	return out
}

// linkOne applies the bin/ rule to one vendor: keep a regular file, else link the PATH entry, else
// drop the link; returns the %-8s status verb and its detail text.
func linkOne(t Tool, bin string, dryRun bool) (status, detail string) {
	dst := filepath.Join(paths.BinDir(), bin)
	c := linkCandidate(t)
	missing := fmt.Sprintf("%s not installed (claude-code install --tools=%s)", bin, t.Name)
	fi, err := os.Lstat(dst)
	if err != nil && !os.IsNotExist(err) {
		return "error", err.Error()
	}
	isLink := err == nil && fi.Mode()&os.ModeSymlink != 0
	switch {
	case err == nil && fi.Mode().IsRegular():
		line := dst + " (installed here)"
		if c != "" {
			line += " · also on PATH: " + c
		}
		return "keep", line
	case err == nil && !isLink:
		return "skip", dst + " (not a file or link)"
	case isLink && c != "" && readlink(dst) == c:
		return "ok", dst + " → " + c
	case c != "":
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
	case isLink:
		// no PATH/local-bin candidate: a symlink to a real, executable, non-shim target is the
		// user's own working link — keep it; only a dangling or shim-pointing link is removed.
		if _, err := paths.VendorBin(t.Name); err == nil {
			return "keep", dst + " → " + readlink(dst) + " (existing link)"
		}
		if !dryRun {
			// re-check: dst may have become a regular file since the classification above.
			if fi, lerr := os.Lstat(dst); lerr == nil && fi.Mode().IsRegular() {
				return "keep", dst + " (installed here)"
			}
			if err := os.Remove(dst); err != nil {
				return "error", err.Error()
			}
		}
		return "remove", dst + "; " + missing
	default:
		return "missing", missing
	}
}

// linkCandidate is the vendor's PATH entry (unresolved, so self-updaters are followed), else ~/.local/bin, else "".
func linkCandidate(t Tool) string {
	if c, err := paths.LookPathReal(t.Binary); err == nil {
		return c
	}
	c := filepath.Join(localBinDir(), t.Binary)
	if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
		return c
	}
	return ""
}

func readlink(p string) string {
	target, _ := os.Readlink(p)
	return target
}

// replaceLink swaps dst for a symlink to target atomically via a temp link renamed over it.
// kept=true means dst turned into a regular file between the caller's check and this call (a
// concurrent direct install winning the race): the temp link is discarded and dst is left alone.
func replaceLink(dst, target string) (kept bool, err error) {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%d.link", filepath.Base(dst), os.Getpid()))
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return false, err
	}
	switch fi, lerr := os.Lstat(dst); {
	case lerr == nil && fi.Mode().IsRegular():
		_ = os.Remove(tmp)
		return true, nil
	case lerr != nil && !os.IsNotExist(lerr):
		_ = os.Remove(tmp)
		return false, lerr
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return false, nil
}
