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

// CodemodeEnv is set by every code-mode launcher (Pi: on|only); no shim, hook or ForModel adds rtk under it.
const CodemodeEnv = "HARNESS_CODEMODE"

// CodeMode reports whether this process runs inside a code-mode agent run.
func CodeMode() bool { return os.Getenv(CodemodeEnv) != "" }

// Names are the shimmed commands: rtk_rewrite_words entries that are real command heads (TestNamesAreRTKWords).
var Names = []string{"curl", "df", "diff", "docker", "du", "find", "gh", "git", "go", "golangci-lint", "grep", "jq",
	"kubectl", "ls", "make", "ps", "rg", "ssh", "stat", "tree", "wc"}

// Dir is the shim directory a wrapped Pi launch puts first on PATH.
func Dir() string { return filepath.Join(paths.StateDir(), "rtkwrap", "bin") }

// Binary is the shim executable every Dir entry links to; `make compile` builds it.
func Binary() string { return filepath.Join(paths.BinDir(), ".rtkwrap") }

// UserBashEnvVar carries the user's own BASH_ENV, which the harness BASH_ENV file sources first.
const UserBashEnvVar = "HARNESS_USER_BASH_ENV"

// bashEnvName is the BASH_ENV file in the shim dir; not executable, so PATH lookup never finds it.
const bashEnvName = ".bash_env"

// bashEnv's EXIT trap stops bash exec'ing a -c line's last command, so the shim sees its tool-shell parent.
const bashEnv = `if [ -n "$` + UserBashEnvVar + `" ]; then . "$` + UserBashEnvVar + `"; [ -n "$(trap -p EXIT)" ] || trap : EXIT; else trap : EXIT; fi
`

// Ensure makes dir hold one symlink per name to bin, replacing a wrong entry, plus the BASH_ENV file.
func Ensure(dir, bin string) error {
	if fi, err := os.Stat(bin); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("shim binary %s is missing or not executable", bin)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := paths.WriteIfChanged(filepath.Join(dir, bashEnvName), []byte(bashEnv), 0o644); err != nil {
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

// Launch puts Dir first on PATH, sets CodemodeEnv and, outside code mode, BASH_ENV; a failure notes, env unchanged.
func Launch(env []string, codemode string, notice io.Writer) []string {
	if codemode != "" { // set even when the shims are unusable: the hook and ForModel read it too
		env = append(slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
			return strings.HasPrefix(kv, CodemodeEnv+"=")
		}), CodemodeEnv+"="+codemode)
	}
	if runtime.GOOS == "windows" {
		return env
	}
	dir := Dir()
	if err := Ensure(dir, Binary()); err != nil {
		fmt.Fprintf(notice, "claude-code: the rtk shims are unusable (%v); launching without them\n", err)
		return env
	}
	path := os.Getenv("PATH")
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v // the last PATH entry is the one the child gets
			return true
		}
		return strings.HasPrefix(kv, CodemodeEnv+"=")
	})
	path = strings.Join(withoutDir(filepath.SplitList(path), dir), string(os.PathListSeparator))
	out = append(out, "PATH="+dir+string(os.PathListSeparator)+path, CodemodeEnv+"="+codemode)
	file := filepath.Join(dir, bashEnvName)
	if codemode != "" {
		return withoutBashEnv(out, file)
	}
	return withBashEnv(out, file)
}

// withoutBashEnv undoes withBashEnv(env, file), inherited or in env: the user's own BASH_ENV (maybe empty) comes back.
func withoutBashEnv(env []string, file string) []string {
	bashEnv, user := os.Getenv("BASH_ENV"), os.Getenv(UserBashEnvVar)
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		if v, ok := strings.CutPrefix(kv, "BASH_ENV="); ok {
			bashEnv = v
			return true
		}
		if v, ok := strings.CutPrefix(kv, UserBashEnvVar+"="); ok {
			user = v
			return true
		}
		return false
	})
	if bashEnv != file {
		return env
	}
	return append(out, "BASH_ENV="+user, UserBashEnvVar+"=") // explicit, so the inherited value never comes back
}

// withBashEnv is env with BASH_ENV=file, the user's own BASH_ENV kept in UserBashEnvVar for file to source.
func withBashEnv(env []string, file string) []string {
	user := os.Getenv("BASH_ENV")
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		v, ok := strings.CutPrefix(kv, "BASH_ENV=")
		if ok {
			user = v
		}
		return ok
	})
	if user != file { // a nested launch inherits ours and keeps the user's value already carried
		out = slices.DeleteFunc(out, func(kv string) bool { return strings.HasPrefix(kv, UserBashEnvVar+"=") })
		if user != "" {
			out = append(out, UserBashEnvVar+"="+user)
		}
	}
	return append(out, "BASH_ENV="+file)
}

// withoutDir is list minus every entry naming dir.
func withoutDir(list []string, dir string) []string {
	return slices.DeleteFunc(slices.Clone(list), func(d string) bool { return d != "" && filepath.Clean(d) == dir })
}
