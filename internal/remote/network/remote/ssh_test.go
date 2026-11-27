package remote

import (
	"encoding/base64"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// decodedScript recovers the shell script hostArgv base64-encoded into ssh's last argument.
func decodedScript(t *testing.T, argv []string) string {
	t.Helper()
	last := argv[len(argv)-1]
	start := strings.Index(last, "printf %s ") + len("printf %s ")
	end := strings.Index(last, " | base64 -d")
	b, err := base64.StdEncoding.DecodeString(last[start:end])
	if err != nil {
		t.Fatalf("script not base64: %v (%q)", err, last)
	}
	return string(b)
}

func TestHostArgvPlainVerb(t *testing.T) {
	argv, err := hostArgv("devhost", []string{"vault", "ls", "it's"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, "-t") || slices.ContainsFunc(argv, func(a string) bool { return a == "-L" }) {
		t.Errorf("want -t and no tunnel: %q", argv)
	}
	if i := slices.Index(argv, "--"); i < 0 || argv[i+1] != "devhost" {
		t.Errorf("host must follow --: %q", argv)
	}
	script := decodedScript(t, argv)
	if !strings.Contains(script, PathPrefix) || !strings.Contains(script, `'it'\''s'`) {
		t.Errorf("script = %q", script)
	}
}

// Bin is $HARNESS_REMOTE_BIN when set, else claude-code on the remote PATH.
func TestBinEnv(t *testing.T) {
	t.Setenv(BinEnv, "")
	if got := Bin(); got != "claude-code" {
		t.Errorf("Bin() = %q, want claude-code", got)
	}
	t.Setenv(BinEnv, "/opt/cc")
	if got := Bin(); got != "/opt/cc" {
		t.Errorf("Bin() with %s set = %q, want /opt/cc", BinEnv, got)
	}
}

func TestSplitHostErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"--host"}, {"--host", "-oProxyCommand=x", "version"}, {"--host", "devhost"},
		{"--host="}} {
		if _, _, err := SplitHost(Flag, args); err == nil {
			t.Errorf("splitHost(%q) accepted", args)
		}
	}
	for _, args := range [][]string{{"--host=devhost", "doctor", "--prompts"}, {"--host", "devhost", "doctor", "--prompts"}} {
		host, rest, err := SplitHost(Flag, args)
		if err != nil || host != "devhost" || !slices.Equal(rest, []string{"doctor", "--prompts"}) {
			t.Errorf("%q: %q %q %v", args, host, rest, err)
		}
	}
}

// TestSSHArgvReuse: ControlMaster options precede "--" by default and drop out under HARNESS_SSH_REUSE=0|off.
func TestSSHArgvReuse(t *testing.T) {
	orig := controlDir
	controlDir = func() string { return "/tmp/cc" }
	t.Cleanup(func() { controlDir = orig })
	reuse := []string{"-o", "ControlMaster=auto", "-o", "ControlPersist=10m", "-o", "ControlPath=/tmp/cc/%C"}
	for _, tc := range []struct {
		env  string
		want bool
	}{{"", true}, {"1", true}, {"0", false}, {"OFF", false}} {
		t.Setenv(reuseEnv, tc.env)
		argv, err := hostArgv("devhost", []string{"version"}, true)
		if err != nil {
			t.Fatal(err)
		}
		dash := slices.Index(argv, "--")
		got := slices.Contains(argv[:dash], "ControlMaster=auto") && slices.Contains(argv[:dash], "ControlPath=/tmp/cc/%C")
		if got != tc.want || !slices.Contains(argv[:dash], "-t") {
			t.Errorf("%s=%q: argv %q, want reuse=%v and -t kept", reuseEnv, tc.env, argv, tc.want)
		}
		if tc.want && !slices.Equal(argv[3:9], reuse) {
			t.Errorf("reuse options = %q, want %q", argv[3:9], reuse)
		}
	}
}

// TestDefaultControlDirShortAndPrivate: a short state dir hosts the sockets (0700); a long one falls back to ~/.ssh/cc.
func TestDefaultControlDirShortAndPrivate(t *testing.T) {
	hosttest.SkipOnWindows(t, "/tmp socket dir")
	short, err := os.MkdirTemp("/tmp", "cc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	t.Setenv("HOME", short)
	t.Setenv("CLAUDE_CONFIG_DIR", short)
	for _, tc := range []struct{ config, want string }{
		{short, filepath.Join(paths.StateDir(), "ssh")},
		{filepath.Join(short, strings.Repeat("x", controlPathMax)), filepath.Join(short, ".ssh", "cc")},
	} {
		t.Setenv("CLAUDE_CONFIG_DIR", tc.config)
		dir := defaultControlDir()
		fi, err := os.Stat(dir)
		if dir != tc.want || err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700) {
			t.Errorf("config %q: control dir %q (want %q), err %v", tc.config, dir, tc.want, err)
		}
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// TestLoginForward: only a browser `databricks login` is pinned to an exact port, browser off, and forwarded.
func TestLoginForward(t *testing.T) {
	cases := []struct {
		in   string
		want string
		port int
	}{
		{"databricks login", "databricks login --port 8031 --no-browser", 8031},
		{"databricks login --force --port=8020", "databricks login --force --port=8020 --no-browser", 8020},
		{"databricks login --no-browser --port 9000 --paste", "databricks login --no-browser --port 9000 --paste", 9000},
		{"databricks login --host-browser --force", "databricks login --force", 0},
		{"databricks login --as sp", "databricks login --as sp", 0},
		{"databricks login --dry-run", "databricks login --dry-run", 0},
		{"databricks login --port nope", "databricks login --port nope", 0},
		{"databricks status", "databricks status", 0},
	}
	orig := freeLocalPort
	freeLocalPort = func(first, _ int) int { return first + 1 } // 8030 busy here: only a login with no --port moves
	t.Cleanup(func() { freeLocalPort = orig })
	for _, c := range cases {
		got, port := loginForward(strings.Fields(c.in))
		if strings.Join(got, " ") != c.want || port != c.port {
			t.Errorf("loginForward(%q) = %q, %d; want %q, %d", c.in, got, port, c.want, c.port)
		}
	}
	argv := withForward([]string{"ssh", "-o", "ControlMaster=auto", "--", "devhost", "x"}, 8030)
	i, m := slices.Index(argv, "-L"), slices.Index(argv, "ControlMaster=no")
	if i < 0 || argv[i+1] != "8030:localhost:8030" || m < 0 || m > slices.Index(argv, "ControlMaster=auto") {
		t.Errorf("forward must precede the reuse options and \"--\": %q", argv)
	}
}

func stubControlSSH(t *testing.T, mode string) (path, log string) {
	t.Helper()
	hosttest.SkipOnWindows(t, "unix socket and shell ssh stub")
	dir, err := os.MkdirTemp("/tmp", "cc-ssh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path, log = filepath.Join(dir, strings.Repeat("a", 40)), filepath.Join(dir, "calls")
	orig := controlDir
	controlDir = func() string { return dir }
	t.Cleanup(func() { controlDir = orig })
	t.Setenv(reuseEnv, "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := "#!/bin/sh\npath=" + textutil.ShellQuote(path) + "\nlog=" + textutil.ShellQuote(log) +
		"\nmode=" + textutil.ShellQuote(mode) + "\n" + `
printf '%s\n' "$*" >> "$log"
case "$1" in
-G)
  [ "$mode" = config-fail ] && exit 255
  [ "$mode" = config-hang ] && exec sleep 5
  printf 'controlpath %s\n' "$path"
  ;;
-O)
  [ "$#" = 8 ] && [ "$2" = check ] && [ "$3" = -o ] && [ "$4" = BatchMode=yes ] &&
    [ "$5" = -S ] && [ "$6" = "$path" ] && [ "$7" = -- ] && [ "$8" = stub-host ] || exit 99
  [ "$mode" = live ] && exit 0
  [ "$mode" = hang ] && exec sleep 5
  [ "$mode" = replace ] && mv "$path.next" "$path"
  exit 255
  ;;
*)
  if [ -S "$path" ]; then sleep 0.25; exit 255; fi
  printf 'fresh master\n'
  ;;
esac
`
	hosttest.WriteExec(t, filepath.Join(dir, "ssh"), script)
	return path, log
}

func controlSocketFixture(t *testing.T, path, kind string) {
	t.Helper()
	switch kind {
	case "absent":
		return
	case "regular":
		hosttest.WriteRel(t, filepath.Dir(path), filepath.Base(path), "not a socket")
		return
	case "symlink":
		if err := os.Symlink(path+".other", path); err != nil {
			t.Fatal(err)
		}
		return
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSSHControlMasterDeadRecovery(t *testing.T) {
	path, log := stubControlSSH(t, "dead")
	const samples = 5
	var baseline, recovered time.Duration
	for range samples {
		controlSocketFixture(t, path, "socket")
		start := time.Now()
		if err := exec.CommandContext(t.Context(), "ssh", "--", "stub-host", "true").Run(); err == nil {
			t.Fatal("negative control unexpectedly bypassed the dead socket")
		}
		baseline += time.Since(start)
		start = time.Now()
		argv, err := StreamArgs("stub-host", "true")
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.CommandContext(t.Context(), argv[0], argv[1:]...).Output()
		recovered += time.Since(start)
		if err != nil || string(out) != "fresh master\n" {
			t.Fatalf("recovery = %q, %v", out, err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dead socket remains: %v", err)
		}
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "-G -o ControlMaster=auto -o ControlPersist=10m -o ControlPath=" + filepath.Join(filepath.Dir(path), "%C") + " -- stub-host"
	if strings.Count(string(calls), want) != samples || strings.Count(string(calls), "-O check -o BatchMode=yes -S "+path+" -- stub-host") != samples {
		t.Fatalf("wrong probe arguments: %s", calls)
	}
	t.Logf("stub dead-socket recovery, n=%d: before mean=%s (250ms simulated stall), after mean=%s", samples, baseline/samples, recovered/samples)
}

func TestSSHControlMasterProbeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		mode                   string
		removed, same, checked bool
	}{
		{"live", false, true, true}, {"dead", true, false, true}, {"hang", true, false, true},
		{"config-fail", false, true, false}, {"config-hang", false, true, false}, {"replace", false, false, true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			path, log := stubControlSSH(t, tc.mode)
			controlSocketFixture(t, path, "socket")
			if tc.mode == "replace" {
				controlSocketFixture(t, path+".next", "socket")
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if _, err := StreamArgs("stub-host", "true"); err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(start)
			after, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) != tc.removed {
				t.Fatalf("removed=%v: stat = %v", tc.removed, err)
			}
			if os.SameFile(before, after) != tc.same {
				t.Fatalf("socket identity changed unexpectedly: %v", err)
			}
			if elapsed > controlCheckTimeout+time.Second {
				t.Fatalf("probe exceeded bounded deadline: %s", elapsed)
			}
			calls, err := os.ReadFile(log)
			if err != nil || strings.Contains(string(calls), "-O check") != tc.checked {
				t.Fatalf("probe not run: %s, %v", calls, err)
			}
			t.Logf("%s probe elapsed=%s", tc.mode, elapsed)
		})
	}
}

func TestSSHControlMasterSkipsUnsafeTargets(t *testing.T) {
	for _, tc := range []struct {
		name, kind, host, reuse string
		other, missing, config  bool
	}{
		{"empty", "absent", "stub-host", "", false, false, false},
		{"regular", "regular", "stub-host", "", true, false, true},
		{"symlink", "symlink", "stub-host", "", true, false, true},
		{"other-host", "absent", "stub-host", "", true, false, true},
		{"disabled", "socket", "stub-host", "off", false, false, false},
		{"invalid-host", "socket", "-oProxyCommand=bad", "", false, false, false},
		{"missing-ssh", "socket", "stub-host", "", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, log := stubControlSSH(t, "dead")
			controlSocketFixture(t, path, tc.kind)
			if tc.other {
				controlSocketFixture(t, path+".other", "socket")
			}
			t.Setenv(reuseEnv, tc.reuse)
			if tc.missing {
				t.Setenv("PATH", t.TempDir())
			}
			_, err := StreamArgs(tc.host, "true")
			if (err != nil) != (tc.name == "invalid-host") {
				t.Fatalf("StreamArgs: %v", err)
			}
			calls, _ := os.ReadFile(log)
			if strings.Contains(string(calls), "-O check") {
				t.Fatalf("unsafe check: %s", calls)
			}
			if strings.Contains(string(calls), "-G ") != tc.config {
				t.Fatalf("unexpected ssh calls: %s", calls)
			}
			_, err = os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) != (tc.kind == "absent") {
				t.Fatalf("removed non-stale target: %v", err)
			}
		})
	}
}
