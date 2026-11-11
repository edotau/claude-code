package remote

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
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
	argv, err := hostArgv("wks", []string{"vault", "ls", "it's"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, "-t") || slices.ContainsFunc(argv, func(a string) bool { return a == "-L" }) {
		t.Errorf("want -t and no tunnel: %q", argv)
	}
	if i := slices.Index(argv, "--"); i < 0 || argv[i+1] != "wks" {
		t.Errorf("host must follow --: %q", argv)
	}
	script := decodedScript(t, argv)
	if !strings.Contains(script, PathPrefix) || !strings.Contains(script, `'it'\''s'`) {
		t.Errorf("script = %q", script)
	}
}

func TestSplitHostErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"--host"}, {"--host", "-oProxyCommand=x", "version"}, {"--host", "wks"},
		{"--host="}} {
		if _, _, err := splitHost(args); err == nil {
			t.Errorf("splitHost(%q) accepted", args)
		}
	}
	for _, args := range [][]string{{"--host=wks", "doctor", "--prompts"}, {"--host", "wks", "doctor", "--prompts"}} {
		host, rest, err := splitHost(args)
		if err != nil || host != "wks" || !slices.Equal(rest, []string{"doctor", "--prompts"}) {
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
		argv, err := hostArgv("wks", []string{"version"}, true)
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
		if dir != tc.want || err != nil || fi.Mode().Perm() != 0o700 {
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
		{"databricks login --as sp", "databricks login --as sp", 0},
		{"databricks login --dry-run", "databricks login --dry-run", 0},
		{"databricks login --port nope", "databricks login --port nope", 0},
		{"databricks status", "databricks status", 0},
		{"vault ls", "vault ls", 0},
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
	argv := withForward([]string{"ssh", "-o", "ControlMaster=auto", "--", "wks", "x"}, 8030)
	i, m := slices.Index(argv, "-L"), slices.Index(argv, "ControlMaster=no")
	if i < 0 || argv[i+1] != "8030:localhost:8030" || m < 0 || m > slices.Index(argv, "ControlMaster=auto") {
		t.Errorf("forward must precede the reuse options and \"--\": %q", argv)
	}
}
