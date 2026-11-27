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
	"sync"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/agentid"
)

// CodemodeEnv (off|on|only) is set by the launcher for a dispatched run; on/only: no shim, hook or ForModel adds rtk.
const CodemodeEnv = "CODEMODE"

// vendorVars are the agents' code-mode variable names, computed once.
var vendorVars = sync.OnceValue(func() []string {
	var out []string
	for _, name := range agentid.All() {
		if v := agentid.Routing(name).CodeMode.Var; v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
})

// CodeMode reports whether this process runs in code mode: CODEMODE on, or an agent's vendor variable on (env only).
func CodeMode() bool { return CodeModeSource() != "" }

// CodeModeSource names the variable that turns code mode on, or "" when it is off.
func CodeModeSource() string {
	if CodemodeOn(os.Getenv(CodemodeEnv)) {
		return CodemodeEnv
	}
	for _, v := range vendorVars() {
		if CodemodeOn(os.Getenv(v)) {
			return v
		}
	}
	return ""
}

// CodemodeOn reports whether a CODEMODE value turns code mode on: on, only, 1 or true.
func CodemodeOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "only", "1", "true":
		return true
	}
	return false
}

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
const bashEnvTrap = `if [ -n "$` + UserBashEnvVar + `" ]; then . "$` + UserBashEnvVar + `"; [ -n "$(trap -p EXIT)" ] || trap : EXIT; else trap : EXIT; fi
`

// bashEnvFor is the BASH_ENV file for dir: the trap, then dir first on PATH again (path_helper).
func bashEnvFor(dir string) string {
	q := textutil.QuoteSH(dir)
	return bashEnvTrap + "case \":$PATH:\" in *:" + q + ":*) ;; *) PATH=" + q + `":$PATH"; export PATH;; esac` + "\n"
}

// Ensure makes dir hold one symlink per name to bin, replacing a wrong entry, plus the BASH_ENV file.
func Ensure(dir, bin string) error {
	if fi, err := os.Stat(bin); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("shim binary %s is missing or not executable", bin)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := paths.WriteIfChanged(filepath.Join(dir, bashEnvName), []byte(bashEnvFor(dir)), 0o644); err != nil {
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
	// set on every path, even with unusable shims, so an inherited CODEMODE never reaches the child
	env = append(slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		return strings.HasPrefix(kv, CodemodeEnv+"=")
	}), CodemodeEnv+"="+codemode)
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
	if CodemodeOn(codemode) {
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
