package rtkwrap

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		if _, reason := tc.c.gate(); (reason == "") != tc.want {
			t.Errorf("%s: gate() reason = %q, want wrap %v", tc.label, reason, tc.want)
		}
	}
}

func TestRewriteWraps(t *testing.T) {
	for _, tc := range []struct {
		rewritten, name string
		args            []string
		want            bool
	}{
		{"rtk git status && rtk ls", "git", []string{"status"}, true},
		{"rtk git status; git push", "git", []string{"status"}, false},
		{"git status", "git", []string{"status"}, false},
		{"", "git", nil, false},
		{"rtk ls -la | head", "ls", []string{"-la"}, true},
		{"rtk git log && git status", "git", []string{"status"}, false},
		{"rtk git log", "git", []string{"status"}, false}, // wraps another git, not this one
	} {
		if got := rewriteWraps(tc.rewritten, tc.name, tc.args); got != tc.want {
			t.Errorf("rewriteWraps(%q, %s) = %v, want %v", tc.rewritten, tc.name, got, tc.want)
		}
	}
}

// TestWrapsVerified rows are the shim's end decision with `rtk rewrite` stubbed to the strings rtk 1.1.0 prints.
func TestWrapsVerified(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	or := rtkRewrite
	t.Cleanup(func() { rtkRewrite = or })
	for _, tc := range []struct {
		label, line, rewritten string
		name                   string
		args                   []string
		want                   bool
	}{
		{"git diff --output", "git diff --output=out.patch", "rtk git diff --output=out.patch", "git", []string{"diff", "--output=out.patch"}, false},
		{"curl -o", "curl -s http://x -o f", "rtk curl -s http://x -o f", "curl", []string{"-s", "http://x", "-o", "f"}, false},
		{"curl -O cluster", "curl -sSLO http://x", "rtk curl -sSLO http://x", "curl", []string{"-sSLO", "http://x"}, false},
		{"wget -O", "wget -O f http://x", "rtk wget -O f http://x", "wget", []string{"-O", "f", "http://x"}, false},
		{"source then git", "source s.sh && git status", "source s.sh && rtk git status", "git", []string{"status"}, false},
		{"dot then git", ". s.sh; git status", ". s.sh; rtk git status", "git", []string{"status"}, false},
		{"eval then git", "eval x; git status", "eval x; rtk git status", "git", []string{"status"}, false},
		{"other invocation wrapped", "git log; git status", "rtk git log; git status", "git", []string{"log"}, false},
		{"vetoed own argv", "F=--porcelain; git status $F", "F=--porcelain; rtk git status --porcelain", "git", []string{"status", "--porcelain"}, false},
		{"single-quoted literal", "echo '$(x)'; git status", "echo '$(x)'; rtk git status", "git", []string{"status"}, true},
		{"plain chain", "git status; ls", "rtk git status; rtk ls", "git", []string{"status"}, true},
		{"pipe to git apply", "git diff | git apply", "", "git", []string{"diff"}, false},
		{"pipe to jq", "git log | jq .", "", "git", []string{"log"}, false},
		{"pipe to while read", "git log | while read l; do echo $l; done", "", "git", []string{"log"}, false},
		{"pipe to xargs", "git ls-files | xargs wc -l", "", "git", []string{"ls-files"}, false},
		{"pipe to patch", "git diff | patch -p1", "", "git", []string{"diff"}, false},
	} {
		rtkRewrite = func(string, string, []string) string { return tc.rewritten }
		stubTree(t, map[int]string{10: "pi", 20: "bash"}, map[int]int{20: 10}, tc.line)
		c := call{name: tc.name, args: tc.args, mode: "on", pipe: true, pid: 99, ppid: 20, sid: 20, cwd: cwd}
		if reason := c.wraps("rtk", nil); (reason == "") != tc.want {
			t.Errorf("%s: wraps reason = %q, want wrap %v", tc.label, reason, tc.want)
		}
	}
}

func TestDebugModeLogsNoArgv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(ModeEnv, "debug")
	if got := wrapWith("git", []string{"log", "--grep=hunter2"}, nil, nil, nil); got != "" {
		t.Errorf("debug wrapWith = %q, want raw exec", got)
	}
	logDecision("debug", "ls", "")
	logDecision("on", "ls", "rtk-declined")
	b, err := os.ReadFile(LogFile())
	must(t, err)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 || strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "--grep") {
		t.Fatalf("log = %q, want 3 lines without argv", b)
	}
	for i, want := range []string{`"name":"git","verdict":"raw"`, `"verdict":"would-wrap"`, `"verdict":"raw","reason":"rtk-declined"`} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %s, want %s", i, lines[i], want)
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
