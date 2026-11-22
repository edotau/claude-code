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

// ModeEnv switches wrapping: "on" wraps, "debug" only logs what would wrap, anything else is off.
const ModeEnv = "RTK_WRAP"

// CodemodeEnv is the variable Pi's launch sets to the effective code mode (on|only); the shim never wraps under it.
const CodemodeEnv = "HARNESS_PI_CODEMODE"

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
	for _, name := range append(slices.Clone(Names), rtkName) {
		link := filepath.Join(dir, name)
		if got, err := os.Readlink(link); err == nil && got == bin {
			continue
		}
		if err := paths.SwapSymlink(bin, link); err != nil {
			return err
		}
	}
	return nil
}

// LaunchEnv is Launch with no code mode.
func LaunchEnv(env []string, mode string, notice io.Writer) []string {
	return Launch(env, mode, "", notice)
}

// Launch is env with Dir first on PATH when mode is on|debug or codemode is set, RTK_WRAP=<mode> and CodemodeEnv set to
// codemode (a stale value dropped); a failure notes once, env unchanged.
func Launch(env []string, mode, codemode string, notice io.Writer) []string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	wrap := mode == "on" || mode == "debug"
	if runtime.GOOS == "windows" || !wrap && codemode == "" {
		return env
	}
	dir := Dir()
	if err := Ensure(dir, Binary()); err != nil {
		fmt.Fprintf(notice, "claude-code: %s=%s codemode=%q but the rtk shims are unusable (%v); launching without them\n", ModeEnv, mode, codemode, err)
		return env
	}
	path := os.Getenv("PATH")
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v // the last PATH entry is the one the child gets
			return true
		}
		return wrap && strings.HasPrefix(kv, ModeEnv+"=") || strings.HasPrefix(kv, CodemodeEnv+"=")
	})
	path = strings.Join(withoutDir(filepath.SplitList(path), dir), string(os.PathListSeparator))
	out = append(out, "PATH="+dir+string(os.PathListSeparator)+path, CodemodeEnv+"="+codemode)
	if wrap {
		out = append(out, ModeEnv+"="+mode)
	}
	return out
}

// withoutDir is list minus every entry naming dir.
func withoutDir(list []string, dir string) []string {
	return slices.DeleteFunc(slices.Clone(list), func(d string) bool { return d != "" && filepath.Clean(d) == dir })
}
