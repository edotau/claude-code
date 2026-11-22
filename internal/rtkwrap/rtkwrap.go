// Package rtkwrap routes a Pi tool shell's commands through rtk via PATH shims; the vetoes are shared with the hook.
package rtkwrap

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// ModeEnv switches wrapping: only "on" wraps; the launch reads it from the process env, then the vault env.
const ModeEnv = "RTK_WRAP"

// Names are the shimmed commands: rtk_rewrite_words entries that are real command heads (TestNamesAreRTKWords).
var Names = []string{"curl", "df", "diff", "docker", "du", "find", "gh", "git", "go", "golangci-lint", "grep", "jq",
	"kubectl", "ls", "make", "ps", "rg", "ssh", "stat", "tree", "wc"}

// Dir is the shim directory a wrapped Pi launch puts first on PATH.
func Dir() string { return filepath.Join(paths.StateDir(), "rtkwrap", "bin") }

// Binary is the shim executable every Dir entry links to; `make compile` builds it.
func Binary() string { return filepath.Join(paths.BinDir(), ".rtkwrap") }

// Ensure makes dir hold one symlink per name to bin, replacing a wrong entry; bin must be an executable file.
func Ensure(dir, bin string) error {
	if fi, err := os.Stat(bin); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("shim binary %s is missing or not executable", bin)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range Names {
		link := filepath.Join(dir, name)
		if got, err := os.Readlink(link); err == nil && got == bin {
			continue
		}
		tmp := link + ".new"
		_ = os.Remove(tmp)
		if err := os.Symlink(bin, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, link); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return nil
}

// LaunchEnv is env with Dir first on PATH and RTK_WRAP=on when mode is on; any failure notes once and leaves env as is.
func LaunchEnv(env []string, mode string, notice io.Writer) []string {
	if runtime.GOOS == "windows" || !strings.EqualFold(strings.TrimSpace(mode), "on") {
		return env
	}
	dir := Dir()
	if err := Ensure(dir, Binary()); err != nil {
		fmt.Fprintf(notice, "claude-code: %s=on but the rtk shims are unusable (%v); launching without them\n", ModeEnv, err)
		return env
	}
	path := os.Getenv("PATH")
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v // the last PATH entry is the one the child gets
			return true
		}
		return strings.HasPrefix(kv, ModeEnv+"=")
	})
	path = strings.Join(withoutDir(filepath.SplitList(path), dir), string(os.PathListSeparator))
	return append(out, "PATH="+dir+string(os.PathListSeparator)+path, ModeEnv+"=on")
}

// withoutDir is list minus every entry naming dir.
func withoutDir(list []string, dir string) []string {
	return slices.DeleteFunc(slices.Clone(list), func(d string) bool { return d != "" && filepath.Clean(d) == dir })
}
