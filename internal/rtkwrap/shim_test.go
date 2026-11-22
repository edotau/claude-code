package rtkwrap

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// stubTree swaps the process-table seams for a fixed tree: pid 10 is Pi, pid 20 its bash tool shell running cmd.
func stubTree(t *testing.T, comms map[int]string, parents map[int]int, cmd string) {
	t.Helper()
	oc, op, oa := procComm, procParent, procArgv
	t.Cleanup(func() { procComm, procParent, procArgv = oc, op, oa })
	procComm = func(pid int) string { return comms[pid] }
	procParent = func(pid int) int { return parents[pid] }
	procArgv = func(pid int) []string { return []string{"/bin/bash", "-c", cmd} }
}

func TestWrapDecision(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	tree := map[int]string{10: "pi", 20: "bash", 30: "bash", 40: "make", 50: "bash"}
	parents := map[int]int{20: 10, 30: 20, 40: 20, 50: 10}
	forked := func(name string, args ...string) call { // a child the tool shell (pid 20, session leader) forked
		return call{name: name, args: args, mode: "on", pipe: true, pid: 99, ppid: 20, sid: 20, cwd: cwd}
	}
	execd := func(name string, args ...string) call { // `bash -c` exec'd: the shim is the session leader under Pi
		return call{name: name, args: args, mode: "on", pipe: true, pid: 20, ppid: 10, sid: 20, cwd: cwd}
	}
	off, tty := forked("git", "status"), forked("git", "status")
	off.mode, tty.pipe = "", false
	footer := execd("git", "status")
	footer.sid = 5 // Pi's own git (footer, packages): not a detached tool shell
	sub, recipe := forked("git", "status"), forked("git", "status")
	sub.ppid, recipe.ppid = 30, 40 // a $(...) subshell's child, a make recipe's child
	attached := forked("git", "status")
	attached.ppid = 50 // a shell Pi ran without detaching: not the tool shell
	for _, tc := range []struct {
		label string
		c     call
		cmd   string
		want  bool
	}{
		{"forked chain", forked("git", "status"), "git status; ls", true},
		{"exec'd single", execd("git", "status", "--short"), "", true},
		{"off", off, "git status", false},
		{"stdout not a pipe", tty, "git status", false},
		{"pi's own git", footer, "", false},
		{"subshell child", sub, "git status", false},
		{"make recipe child", recipe, "git status", false},
		{"attached shell child", attached, "git status", false},
		{"command substitution", forked("git", "rev-parse", "HEAD"), "x=$(git rev-parse HEAD); echo $x", false},
		{"value subcommand", execd("git", "rev-parse", "HEAD"), "", false},
		{"machine flag", forked("git", "status"), "git status --porcelain", false},
		{"commit", execd("git", "commit", "-m", "x"), "", false},
		{"PATH altered", forked("git", "status"), "PATH=/x:$PATH; git status", false},
		{"go vet", execd("go", "vet", "./..."), "", false},
	} {
		stubTree(t, tree, parents, tc.cmd)
		if _, got := tc.c.command(); got != tc.want {
			t.Errorf("%s: command() = %v, want %v", tc.label, got, tc.want)
		}
	}
}

func TestRewriteWraps(t *testing.T) {
	for _, tc := range []struct {
		rewritten, name string
		want            bool
	}{
		{"rtk git status && rtk ls", "git", true},
		{"rtk git status; git push", "git", false},
		{"git status", "git", false},
		{"", "git", false},
		{"rtk ls -la | head", "ls", true},
	} {
		if got := rewriteWraps(tc.rewritten, tc.name); got != tc.want {
			t.Errorf("rewriteWraps(%q, %s) = %v, want %v", tc.rewritten, tc.name, got, tc.want)
		}
	}
}

func TestResolveSkipsShimDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shims are unix-only")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	self := filepath.Join(root, "shim")
	alias, real := filepath.Join(root, "alias"), filepath.Join(root, "real")
	must(t, os.MkdirAll(real, 0o755))
	must(t, os.MkdirAll(alias, 0o755))
	must(t, os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755))
	must(t, os.WriteFile(filepath.Join(real, "git"), []byte("#!/bin/sh\n"), 0o755))
	must(t, Ensure(Dir(), self))
	must(t, os.Symlink(self, filepath.Join(alias, "git"))) // the shim under another dir spelling
	fi, err := os.Stat(self)
	must(t, err)
	got, kept := resolve([]string{Dir(), alias, "relative", real, "/nonexistent"}, "git", fi)
	if want := filepath.Join(real, "git"); got != want {
		t.Errorf("resolve = %q, want %q", got, want)
	}
	if want := []string{"relative", real, "/nonexistent"}; len(kept) != len(want) || kept[0] != want[0] || kept[1] != want[1] {
		t.Errorf("kept PATH = %q, want %q (shim dirs dropped)", kept, want)
	}
	if got, _ := resolve([]string{Dir(), alias}, "git", fi); got != "" {
		t.Errorf("shim-only PATH resolved %q, want none", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
