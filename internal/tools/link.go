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
	for _, t := range Tools {
		bin, ok := paths.VendorName(t.Name)
		if !ok || (len(names) > 0 && !slices.Contains(names, t.Name)) {
			continue
		}
		linkOne(w, t, bin, dryRun)
	}
}

// linkOne applies the bin/ rule to one vendor: keep a regular file, else link the PATH entry, else drop the link.
func linkOne(w io.Writer, t Tool, bin string, dryRun bool) {
	dst := filepath.Join(paths.BinDir(), bin)
	c := linkCandidate(t)
	missing := fmt.Sprintf("%s not installed (claude-code install --tools=%s)", bin, t.Name)
	fi, err := os.Lstat(dst)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(w, "  %-8s %v\n", "error", err)
		return
	}
	isLink := err == nil && fi.Mode()&os.ModeSymlink != 0
	switch {
	case err == nil && fi.Mode().IsRegular():
		line := dst + " (installed here)"
		if c != "" {
			line += " · also on PATH: " + c
		}
		fmt.Fprintf(w, "  %-8s %s\n", "keep", line)
	case err == nil && !isLink:
		fmt.Fprintf(w, "  %-8s %s (not a file or link)\n", "skip", dst)
	case isLink && c != "" && readlink(dst) == c:
		fmt.Fprintf(w, "  %-8s %s → %s\n", "ok", dst, c)
	case c != "":
		suffix := ""
		if npmPrefixFromBinary(c) != "" {
			suffix = " (npm-global)"
		}
		fmt.Fprintf(w, "  %-8s %s → %s%s\n", "link", dst, c, suffix)
		if !dryRun {
			if err := replaceLink(dst, c); err != nil {
				fmt.Fprintf(w, "  %-8s %v\n", "error", err)
			}
		}
	case isLink:
		fmt.Fprintf(w, "  %-8s %s; %s\n", "remove", dst, missing)
		if !dryRun {
			if err := os.Remove(dst); err != nil {
				fmt.Fprintf(w, "  %-8s %v\n", "error", err)
			}
		}
	default:
		fmt.Fprintf(w, "  %-8s %s\n", "missing", missing)
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
func replaceLink(dst, target string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%d.link", filepath.Base(dst), os.Getpid()))
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
