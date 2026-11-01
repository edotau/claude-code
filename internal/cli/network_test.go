package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/secretstore"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestNetworkBareAndHelpPrintUsageExitZero(t *testing.T) {
	for _, args := range [][]string{nil, {"-h"}, {"--help"}} {
		code, _, stderr := captureOutput(t, func() int { return cmdNetwork(args) })
		if code != 0 {
			t.Errorf("network %v: exit %d, want 0", args, code)
		}
		for _, sub := range []string{"tailscale", "browser", "websearch", "mcp", "page", "view", "vscode"} {
			if !strings.Contains(stderr, sub) {
				t.Errorf("network %v: help lacks %q: %q", args, sub, stderr)
			}
		}
	}
}

func TestNetworkUnknownSubverb(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdNetwork([]string{"bogus"}) })
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, `unknown subcommand "bogus"`) || !strings.Contains(stderr, "tailscale|browser|websearch|mcp|page|view|vscode") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestNetworkOldSubverbsRejected(t *testing.T) {
	for _, old := range []string{"web", "doctor", "status", "connect", "browsers"} {
		if code, _, _ := captureOutput(t, func() int { return cmdNetwork([]string{old}) }); code != 1 {
			t.Errorf("network %s: exit %d, want 1", old, code)
		}
	}
}

// TestNetworkSubcommandsRejectUnknownFormat: browsers/setup/view all validate --format through
// helptext.CheckFormat before doing any work — an unknown value is a usage error (2).
func TestNetworkSubcommandsRejectUnknownFormat(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		name string
		fn   func() int
	}{
		{"browser", func() int { return cmdNetworkBrowser([]string{"--format", "yaml"}) }},
		{"browser --setup --dry-run", func() int { return cmdNetworkBrowser([]string{"--setup", "--dry-run", "--format", "yaml"}) }},
		{"view", func() int { return cmdNetworkView([]string{"agent-id", "--format", "yaml"}) }},
	}
	for _, c := range cases {
		code, _, stderr := captureOutput(t, c.fn)
		if code != 2 {
			t.Errorf("network %s: code = %d, want 2; stderr=%q", c.name, code, stderr)
		}
	}
}

// TestNetworkBrowserSetupOnlyFlags: --browser and --dry-run belong to --setup; alone they are usage errors.
func TestNetworkBrowserSetupOnlyFlags(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{{"--browser", "chrome"}, {"--dry-run"}} {
		code, _, stderr := captureOutput(t, func() int { return cmdNetworkBrowser(args) })
		if code != 2 || !strings.Contains(stderr, "belongs to --setup") {
			t.Errorf("%v: code=%d stderr=%q, want 2 + belongs to --setup", args, code, stderr)
		}
	}
}

// TestCmdWebSearchQueryForms: positional and --query both reach OneShot (no key → 1); both at once is usage (2).
func TestCmdWebSearchQueryForms(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"positional", []string{"foo"}, 1},
		{"positional multi-word", []string{"foo", "bar"}, 1},
		{"flag", []string{"--query", "foo"}, 1},
		{"flag and positional", []string{"--query", "a", "b"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No key anywhere: env empty, no keyring tool on PATH, empty config dir — so no network call.
			t.Setenv("TAVILY_API_KEY", "")
			t.Setenv("PATH", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			if got := cmdNetworkWebsearch(tc.args); got != tc.want {
				t.Fatalf("cmdNetworkWebsearch(%q) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// isolateRouterState points the router state dir at a temp dir so no real daemon is ever seen.
func isolateRouterState(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
}

func decodeRestart(t *testing.T, out string) restartRow {
	t.Helper()
	var doc struct {
		Daemons []restartRow `json:"daemons"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Daemons) != 1 {
		t.Fatalf("json = %q (%v)", out, err)
	}
	return doc.Daemons[0]
}

func TestRestartStoppedRouterIsSkipped(t *testing.T) {
	for _, args := range [][]string{{"--format", "json"}, {"--dry-run", "--format", "json"}} {
		isolateRouterState(t)
		var code int
		out := captureStdout(t, func() { code = cmdRestart(args) })
		if code != 0 {
			t.Fatalf("%v: exit = %d, want 0 (a stopped daemon is not a failure)", args, code)
		}
		if row := decodeRestart(t, out); row.Daemon != "proxy" || row.Status != "skipped" || row.OldPID != 0 || row.NewPID != 0 {
			t.Errorf("%v: row = %+v", args, row)
		}
		if _, err := os.Stat(filepath.Join(paths.StateDir(), "router.json")); err == nil {
			t.Errorf("%v: a skipped restart created router.json", args)
		}
	}
}

func TestRestartDryRunRunningRouterChangesNothing(t *testing.T) {
	isolateRouterState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	state := filepath.Join(paths.StateDir(), "router.json")
	doc := fmt.Sprintf(`{"pid":%d,"port":%d}`, os.Getpid(), port)
	if err := os.WriteFile(state, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var code int
	out := captureStdout(t, func() { code = cmdRestart([]string{"--dry-run", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if row := decodeRestart(t, out); row.Status != "would restart" || row.OldPID != os.Getpid() || row.NewPID != 0 {
		t.Errorf("row = %+v", row)
	}
	if b, _ := os.ReadFile(state); string(b) != doc {
		t.Errorf("dry-run rewrote router.json: %q", b)
	}
}

func TestRestartRejectsBadFormat(t *testing.T) {
	isolateRouterState(t)
	if code := cmdRestart([]string{"--format", "yaml"}); code == 0 {
		t.Fatal("bad --format accepted")
	}
}

func TestNetworkVSCodeHelpListsFlags(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdNetwork([]string{"vscode", "-h"}) })
	if code != 0 {
		t.Errorf("network vscode -h: exit %d, want 0", code)
	}
	for _, want := range []string{"network vscode", "folder", "format", "funnel", "no-tailscale", "port", "status", "stop"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("network vscode -h lacks %q: %q", want, stderr)
		}
	}
}

func TestThemeVSCodeServeNoLongerRoutes(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdVSCode([]string{"serve"}) })
	if code != 1 {
		t.Errorf("theme vscode serve: exit %d, want 1", code)
	}
	if !strings.Contains(stderr, `unexpected argument "serve"`) || !strings.Contains(stderr, "network vscode") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestNetworkHelpVSCodeRowFitsEightyColumns(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return cmdNetwork(nil) })
	found := false
	for l := range strings.SplitSeq(stderr, "\n") {
		if strings.HasPrefix(l, "  vscode ") {
			found = true
			if n := len([]rune(l)); n > 80 {
				t.Errorf("vscode row is %d columns: %q", n, l)
			}
		}
	}
	if !found {
		t.Errorf("network help has no vscode row: %q", stderr)
	}
}

// hermeticDispatch isolates tailscale credentials/config so ResolveHost falls through to its
// pass-the-host-through branch instead of hitting a real tailnet.
func hermeticDispatch(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, n := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY"} {
		t.Setenv(n, "")
	}
}

// emptyPATH points PATH at an empty directory: no ssh, no tailscale, no tmux resolvable.
func emptyPATH(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Setenv("PATH", t.TempDir())
}

// fakeSSHOnPATH puts an `ssh` script on PATH that returns canned output for --list's tmux listing.
func fakeSSHOnPATH(t *testing.T, listOutput string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$2\" in *tmux\\ list-sessions*) printf '%s' " + shQuote(listOutput) + " ;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// fakeSSHStatusOnPATH puts an `ssh` script on PATH that returns canned output for the pane-capture
// command (tmux list-panes) used by --status, independent of fakeSSHOnPATH's list-sessions case.
func fakeSSHStatusOnPATH(t *testing.T, captureOutput string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$2\" in *tmux\\ list-panes*) printf '%s' " + shQuote(captureOutput) + " ;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestDispatchMissingHost(t *testing.T) {
	hermeticDispatch(t)
	code, _, stderr := captureOutput(t, func() int { return cmdNetworkRemote([]string{"hi"}) })
	if code == 0 {
		t.Fatalf("missing --host: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "--host") {
		t.Errorf("stderr = %q, want it to mention --host", stderr)
	}
}

func TestDispatchMissingTask(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, _, stderr := captureOutput(t, func() int { return cmdNetworkRemote([]string{"--host", "myhost"}) })
	if code == 0 {
		t.Fatalf("missing task: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "task") {
		t.Errorf("stderr = %q, want it to mention task", stderr)
	}
}

func TestDispatchUnknownAgent(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, _, stderr := captureOutput(t, func() int {
		return cmdNetworkRemote([]string{"--host", "myhost", "--agent", "bash", "hi"})
	})
	if code == 0 {
		t.Fatalf("unknown --agent: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "--agent") {
		t.Errorf("stderr = %q, want it to mention --agent", stderr)
	}
}

// TestDispatchDryRunSpawnsNoSSH is the security-relevant assertion: PATH has no ssh binary at all,
// so if --dry-run ever reached exec.CommandContext it would fail with "executable file not found".
// Exit 0 here proves the ssh/tmux round trip never happened.
func TestDispatchDryRunSpawnsNoSSH(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, out, stderr := captureOutput(t, func() int {
		return cmdNetworkRemote([]string{"--host", "myhost", "--dir", "/tmp/work dir", "--name", "cc-claude-abcd", "--dry-run", "do", "the", "thing"})
	})
	if code != 0 {
		t.Fatalf("dry-run exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "argv[0]: ssh") {
		t.Errorf("dry-run output missing argv[0] line:\n%s", out)
	}
	if !strings.Contains(out, "argv[1]: myhost") {
		t.Errorf("dry-run output missing resolved host in argv[1]:\n%s", out)
	}
	if !strings.Contains(out, "attach: ssh -t myhost tmux attach -t cc-claude-abcd") {
		t.Errorf("dry-run output missing attach hint:\n%s", out)
	}
	// The task given as multiple positional words must be joined and passed as one shell-quoted unit.
	if !strings.Contains(out, "'do the thing'") {
		t.Errorf("dry-run output does not show the joined, quoted task:\n%s", out)
	}
}

func TestDispatchDryRunRejectsBadName(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, _, stderr := captureOutput(t, func() int {
		return cmdNetworkRemote([]string{"--host", "myhost", "--name", "a b", "--dry-run", "hi"})
	})
	if code == 0 {
		t.Fatalf("bad --name: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "--name") {
		t.Errorf("stderr = %q, want it to mention --name", stderr)
	}
}

func TestDispatchListTextAndJSON(t *testing.T) {
	hermeticDispatch(t)
	listing := "cc-claude-ab12|1700000000|1|claude|claude\n"
	fakeSSHOnPATH(t, listing)
	code, out, stderr := captureOutput(t, func() int { return cmdNetworkRemote([]string{"--list", "--host", "myhost"}) })
	if code != 0 {
		t.Fatalf("--list exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "cc-claude-ab12") {
		t.Errorf("--list text output missing session name:\n%s", out)
	}
	if !strings.Contains(out, "cmd=claude") {
		t.Errorf("--list text output missing parsed CurrentCmd field (fieldSep regression):\n%s", out)
	}

	code, out, stderr = captureOutput(t, func() int {
		return cmdNetworkRemote([]string{"--list", "--host", "myhost", "--format", "json"})
	})
	if code != 0 {
		t.Fatalf("--list --format json exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, `"Name"`) || !strings.Contains(out, "cc-claude-ab12") {
		t.Errorf("--list json output = %q, want it to contain the session as JSON", out)
	}
}

func TestDispatchListEmptyIsNotAnError(t *testing.T) {
	hermeticDispatch(t)
	fakeSSHOnPATH(t, "")
	code, out, stderr := captureOutput(t, func() int { return cmdNetworkRemote([]string{"--list", "--host", "myhost"}) })
	if code != 0 {
		t.Fatalf("--list (no sessions) exit %d, want 0; stderr=%q", code, stderr)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("--list (no sessions) output = %q, want empty", out)
	}
}

func TestDispatchAttachRejectsBadSessionName(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, _, stderr := captureOutput(t, func() int {
		return cmdNetworkRemote([]string{"--attach", "--host", "myhost", "../etc"})
	})
	if code == 0 {
		t.Fatalf("--attach with bad session name: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "session") {
		t.Errorf("stderr = %q, want it to mention session", stderr)
	}
}

func TestDispatchAttachMissingSessionArg(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, _, stderr := captureOutput(t, func() int {
		return cmdNetworkRemote([]string{"--attach", "--host", "myhost"})
	})
	if code == 0 {
		t.Fatalf("--attach with no session name: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "session name") {
		t.Errorf("stderr = %q, want it to mention session name", stderr)
	}
}

// statusCaptureFrame builds one captureCommand-shaped record (control-byte framed, "|"-separated
// header) matching dispatch's actual on-wire format, so the fake ssh script exercises the real parser.
func statusCaptureFrame(sess, pid, agent, cmd, text string) string {
	return "\x01" + sess + "|" + pid + "|" + agent + "|" + cmd + "\n" + text + "\x02\n"
}

func TestDispatchListStatusTextAndJSON(t *testing.T) {
	hermeticDispatch(t)
	capture := statusCaptureFrame("cc-claude-ab12", "%1", "claude", "claude", "thinking...")
	fakeSSHStatusOnPATH(t, capture)
	code, out, stderr := captureOutput(t, func() int {
		return cmdNetworkRemote([]string{"--list", "--host", "myhost", "--status"})
	})
	if code != 0 {
		t.Fatalf("--list --status exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "cc-claude-ab12") || !strings.Contains(out, "state=working") {
		t.Errorf("--list --status text output missing session/state:\n%s", out)
	}

	code, out, stderr = captureOutput(t, func() int {
		return cmdNetworkRemote([]string{"--list", "--host", "myhost", "--status", "--format", "json"})
	})
	if code != 0 {
		t.Fatalf("--list --status --format json exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, `"state": "working"`) {
		t.Errorf("--list --status json output = %q, want a classified working state", out)
	}
}

func TestDispatchListStatusNoPanesIsEmptyNotError(t *testing.T) {
	hermeticDispatch(t)
	fakeSSHStatusOnPATH(t, "")
	code, out, stderr := captureOutput(t, func() int {
		return cmdNetworkRemote([]string{"--list", "--host", "myhost", "--status"})
	})
	if code != 0 {
		t.Fatalf("--list --status (no panes) exit %d, want 0; stderr=%q", code, stderr)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("--list --status (no panes) output = %q, want empty", out)
	}
}

func TestDispatchListStatusSSHFailure(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t) // no ssh on PATH at all: the capture round trip itself fails
	code, _, stderr := captureOutput(t, func() int {
		return cmdNetworkRemote([]string{"--list", "--host", "myhost", "--status"})
	})
	if code == 0 {
		t.Fatalf("--list --status with no ssh available: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "dispatch") {
		t.Errorf("stderr = %q, want it to report the capture failure", stderr)
	}
}

// TestDispatchLocalhostDryRunNeverLaunches pins the local branch of ExecArgs (host "localhost" runs
// under "sh -c", not ssh): even with real-looking sh/tmux stubs on PATH, --dry-run must only print
// the resolved argv and never invoke either stub — the marker file must stay absent.
func TestDispatchLocalhostDryRunNeverLaunches(t *testing.T) {
	hermeticDispatch(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	for _, name := range []string{"sh", "tmux"} {
		script := "#!/bin/sh\ntouch " + shQuote(marker) + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	code, out, stderr := captureOutput(t, func() int {
		return cmdNetworkRemote([]string{"--host", "localhost", "task", "--dry-run"})
	})
	if code != 0 {
		t.Fatalf("dry-run exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "argv[0]: sh") {
		t.Errorf("dry-run output missing argv[0]: sh:\n%s", out)
	}
	if !strings.Contains(out, "'task'") {
		t.Errorf("dry-run output missing the quoted task:\n%s", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker exists — a stubbed sh/tmux was actually launched, stat err=%v", err)
	}
}

func TestDispatchHelp(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdNetworkRemote(nil) })
	if code != 0 {
		t.Fatalf("no args: code = %d, want 0", code)
	}
	if !strings.Contains(stderr, "usage: claude-code network remote") {
		t.Errorf("stderr = %q, want usage text", stderr)
	}
}

type tailscaleTestTransport func(*http.Request) (*http.Response, error)

func (f tailscaleTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// fakeTailscaleOnPATH puts a `tailscale` script on PATH answering `status --json` per state.
func fakeTailscaleOnPATH(t *testing.T, backendState string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = status ]; then /bin/cat <<EOF\n" +
		`{"BackendState":"` + backendState + `","Self":{"DNSName":"test-mac.tailxxxxx.ts.net.","AllowedIPs":[]},"Peer":{}}` +
		"\nEOF\nfi\n"
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// fakeTailscaleUpOnPATH puts a `tailscale` script on PATH that answers `status --json` as Stopped and
// records the argv of any `up` invocation into argvFile — the seam item 7's tests drive to prove the
// auth key never rides on argv.
func fakeTailscaleUpOnPATH(t *testing.T, argvFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = status ]; then /bin/cat <<EOF\n" +
		`{"BackendState":"Stopped","Self":{"DNSName":"test-mac.tailxxxxx.ts.net.","AllowedIPs":[]},"Peer":{}}` +
		"\nEOF\nfi\n" +
		"if [ \"$1\" = up ]; then printf '%s\\n' \"$@\" > " + shQuote(argvFile) + "; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func hermeticTailscaleSecrets(t *testing.T) {
	t.Helper()
	vaulttest.Use(t)
	for _, n := range []string{
		"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY", "TAILSCALE_AUTHKEY",
	} {
		t.Setenv(n, "")
	}
}

func TestTailscaleStatusNoCLI(t *testing.T) {
	hermeticTailscaleSecrets(t)
	t.Setenv("PATH", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale(nil) })
	if code == 0 {
		t.Errorf("status with no CLI on PATH: code = 0, want nonzero; stderr=%q", stderr)
	}
}

func TestTailscaleStatusRunningTextAndJSON(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := captureOutput(t, func() int { return cmdNetworkTailscale(nil) })
	if code != 0 {
		t.Fatalf("status exit %d", code)
	}
	if !strings.Contains(out, "test-mac.tailxxxxx.ts.net.") {
		t.Errorf("status text output missing node name:\n%s", out)
	}

	code, out, _ = captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--format", "json"}) })
	if code != 0 || !strings.Contains(out, "test-mac.tailxxxxx.ts.net.") {
		t.Errorf("status --format json: code=%d out=%q", code, out)
	}
}

func TestTailscaleConnectWithoutKeyAlreadyRunningExitsZero(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--connect"}) })
	if code != 0 {
		t.Errorf("connect (already running, no key) exit %d, want 0", code)
	}
	if !strings.Contains(out, "already connected") {
		t.Errorf("connect output = %q, want it to say already connected", out)
	}
}

func TestTailscaleConnectWithoutKeyNotRunningExitsTwo(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Stopped")
	code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--connect"}) })
	if code != 2 {
		t.Errorf("connect (not running, no key) exit %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "configure") && !strings.Contains(stderr, "auth-key") {
		t.Errorf("stderr = %q, want a configure/--auth-key hint", stderr)
	}
}

func TestTailscaleConnectUsesVaultAuthKey(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Stopped")
	if err := secretstore.Store("TAILSCALE_AUTHKEY", "tskey-auth-fake"); err != nil {
		t.Fatal(err)
	}
	_, out, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--connect"}) })
	if strings.Contains(out+stderr, "no auth key given") {
		t.Errorf("connect did not use the vault auth key: stdout=%q stderr=%q", out, stderr)
	}
}

// TestTailscaleConnectAuthKeyDashReadsStdin: `--connect --auth-key -` reads the key from stdin and the
// underlying `tailscale up` never sees it on argv (it goes through tailscale.Connect's --auth-key=file:).
func TestTailscaleConnectAuthKeyDashReadsStdin(t *testing.T) {
	hermeticTailscaleSecrets(t)
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	fakeTailscaleUpOnPATH(t, argvFile)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("tskey-auth-fromstdin\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	code, out, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--connect", "--auth-key", "-"}) })
	if code != 0 {
		t.Fatalf("connect --auth-key - exit %d; out=%q stderr=%q", code, out, stderr)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("tailscale up was not invoked: %v", err)
	}
	if strings.Contains(string(argv), "tskey-") {
		t.Fatalf("auth key leaked into up argv: %q", argv)
	}
	if !strings.Contains(string(argv), "--auth-key=file:") {
		t.Fatalf("up argv = %q, want --auth-key=file:<path>", argv)
	}
}

// TestTailscaleConnectLiteralAuthKeyWarns: a literal (non "-") --auth-key still connects, but warns
// that it is visible in shell history and `ps`; the key still never reaches `tailscale up`'s argv —
// Connect always routes it through --auth-key=file:.
func TestTailscaleConnectLiteralAuthKeyWarns(t *testing.T) {
	hermeticTailscaleSecrets(t)
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	fakeTailscaleUpOnPATH(t, argvFile)

	code, _, stderr := captureOutput(t, func() int {
		return cmdNetworkTailscale([]string{"--connect", "--auth-key", "tskey-auth-literal"})
	})
	if code != 0 {
		t.Fatalf("connect exit %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "visible in shell") {
		t.Errorf("stderr = %q, want a warning about a literal --auth-key", stderr)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("tailscale up was not invoked: %v", err)
	}
	if strings.Contains(string(argv), "tskey-") {
		t.Fatalf("literal auth key still leaked into up argv: %q", argv)
	}
}

func TestTailscaleKeySaveKeepsStdoutPipeable(t *testing.T) {
	hermeticTailscaleSecrets(t)
	for _, save := range []bool{false, true} {
		name := "without_save"
		if save {
			name = "with_save"
		}
		t.Run(name, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			t.Setenv("TAILSCALE_API_KEY", "test-api-key")
			const key = "tskey-auth-xyz"
			// Reuse the CreateAuthKey response pattern from internal/network/tailscale/api_test.go.
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v2/tailnet/-/keys" {
					t.Errorf("unexpected admin API request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				if err := json.NewEncoder(w).Encode(tailscale.AuthKey{ID: "k1", Key: key}); err != nil {
					t.Error(err)
				}
			}))
			defer ts.Close()
			originalClient := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = originalClient })
			http.DefaultClient = &http.Client{Transport: tailscaleTestTransport(func(r *http.Request) (*http.Response, error) {
				req := r.Clone(r.Context())
				req.URL.Scheme = "http"
				req.URL.Host = strings.TrimPrefix(ts.URL, "http://")
				return ts.Client().Do(req)
			})}
			args := []string{"--key"}
			if save {
				args = append(args, "--save")
			}
			code, out, stderr := captureOutput(t, func() int { return cmdNetworkTailscale(args) })
			if code != 0 {
				t.Fatalf("key exit %d, want 0; stderr=%q", code, stderr)
			}
			if out != key+"\n" {
				t.Errorf("stdout = %q, want only %q", out, key+"\n")
			}
			stored, source, ok := secretstore.Get("TAILSCALE_AUTHKEY")
			if save {
				if !ok || stored != key || source != "vault" {
					t.Errorf("saved key = %q, source=%q, found=%v", stored, source, ok)
				}
				want := "saved TAILSCALE_AUTHKEY to the vault (" + secretstore.Redact(key) + ")\n"
				if stderr != want {
					t.Errorf("stderr = %q, want %q", stderr, want)
				}
			} else {
				if ok {
					t.Error("key without --save must not store TAILSCALE_AUTHKEY")
				}
				if stderr != "" {
					t.Errorf("key without --save stderr = %q, want empty", stderr)
				}
			}
		})
	}
}

func TestTailscaleKeyWithoutCredentialFails(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--key"}) })
	if code == 0 {
		t.Fatalf("key with no admin credential: exit 0, want nonzero; stdout=%q", out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("key must print nothing to stdout on failure, got %q", out)
	}
	if !strings.Contains(stderr, "credential") {
		t.Errorf("stderr = %q, want it to mention the missing credential", stderr)
	}
}

func TestTailscaleExitNodeApproveWithoutCredentialPrintsAdminURL(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--exit-node", "approve"}) })
	if code != 2 {
		t.Errorf("exit-node --approve (no credential) exit %d, want 2", code)
	}
	if !strings.Contains(out, "login.tailscale.com/admin/machines") {
		t.Errorf("output = %q, want the admin console URL", out)
	}
}

func TestNetworkTailscaleModeParsing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"two modes", []string{"--machines", "--connect"}, "mutually exclusive"},
		{"key and exit-node", []string{"--key", "--exit-node", "on"}, "mutually exclusive"},
		{"empty exit-node value", []string{"--exit-node", ""}, `must be on, off or approve, got ""`},
		{"machines with empty exit-node", []string{"--machines", "--exit-node", ""}, "mutually exclusive"},
		{"bad exit-node value", []string{"--exit-node", "sideways"}, "must be on, off or approve"},
		{"key flag with connect", []string{"--connect", "--expiry", "1d"}, "--expiry belongs to --key"},
		{"key flag with no mode", []string{"--expiry", "1d"}, "--expiry belongs to --key"},
		{"connect flag with key", []string{"--key", "--auth-key", "x"}, "belongs to --connect"},
		{"advertise-exit with no mode", []string{"--advertise-exit"}, "belongs to --connect"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			fakeTailscaleOnPATH(t, "Running")
			code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale(tc.args) })
			if code != 2 {
				t.Errorf("exit %d, want 2; stderr=%q", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q lacks %q", stderr, tc.want)
			}
		})
	}
}

func TestNetworkTailscaleExitNodeValuesAccepted(t *testing.T) {
	for _, v := range []string{"on", "off"} {
		t.Run(v, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			fakeTailscaleOnPATH(t, "Running")
			_, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--exit-node", v}) })
			if strings.Contains(stderr, "must be on, off or approve") || strings.Contains(stderr, "mutually exclusive") {
				t.Errorf("--exit-node %s rejected by validation: %q", v, stderr)
			}
		})
	}
}

func TestNetworkTailscaleMachinesIsAMode(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	_, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--machines"}) })
	if strings.Contains(stderr, "belongs to") || strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("--machines alone is a valid mode: %q", stderr)
	}
}

func TestNetworkTailscaleRouteEmptyValueExitsTwo(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--route", ""}) })
	if code != 2 {
		t.Errorf("--route with empty value: exit %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "requires a peer name") {
		t.Errorf("stderr = %q, want a message about requiring a peer name", stderr)
	}
}

func TestNetworkTailscaleRouteValueStartingWithDashExitsTwo(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--route", "-invalid"}) })
	if code != 2 {
		t.Errorf("--route with dash-value: exit %d, want 2; stderr=%q", code, stderr)
	}
}

func TestNetworkTailscaleRouteMutualExclusivity(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"route and machines", []string{"--route", "off", "--machines"}},
		{"route and connect", []string{"--route", "peer", "--connect"}},
		{"route and key", []string{"--route", "off", "--key"}},
		{"route and exit-node", []string{"--route", "peer", "--exit-node", "on"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			fakeTailscaleOnPATH(t, "Running")
			code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale(tc.args) })
			if code != 2 {
				t.Errorf("exit %d, want 2; stderr=%q", code, stderr)
			}
			if !strings.Contains(stderr, "mutually exclusive") {
				t.Errorf("stderr = %q, want it to mention mutual exclusion", stderr)
			}
		})
	}
}

func TestNetworkTailscaleRouteRunsAndReports(t *testing.T) {
	for args, want := range map[string]string{"off": "exit node cleared", "my-peer": "routing via my-peer"} {
		t.Run(args, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			fakeTailscaleOnPATH(t, "Running")
			code, stdout, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--route", args}) })
			if code != 0 || !strings.Contains(stdout, want) {
				t.Errorf("--route %s: exit %d stdout %q stderr %q, want 0 and %q", args, code, stdout, stderr, want)
			}
		})
	}
}

func TestNetworkTailscaleSubnetParsing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"empty value", []string{"--subnet", ""}, `must be a CIDR list, approve or off, got ""`},
		{"not a cidr", []string{"--subnet", "sideways"}, "must be a CIDR list, approve or off"},
		{"bad second cidr", []string{"--subnet", "192.168.1.0/24,nope"}, "must be a CIDR list, approve or off"},
		{"subnet and exit-node", []string{"--subnet", "192.168.1.0/24", "--exit-node", "on"}, "mutually exclusive"},
		{"subnet and route", []string{"--subnet", "approve", "--route", "peer"}, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			fakeTailscaleOnPATH(t, "Running")
			code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale(tc.args) })
			if code != 2 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d stderr %q, want 2 and %q", code, stderr, tc.want)
			}
		})
	}
}

func TestNetworkTailscaleSubnetApproveWithoutCredentialPrintsAdminURL(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--subnet", "approve"}) })
	if code != 2 || !strings.Contains(out, "login.tailscale.com/admin/machines") {
		t.Errorf("--subnet approve (no credential): exit %d stdout %q, want 2 and the admin URL", code, out)
	}
}

func TestNetworkTailscaleSubnetAdvertiseAndOffReport(t *testing.T) {
	for args, want := range map[string]string{
		"192.168.1.0/24,10.9.0.0/16": "advertising 192.168.1.0/24, 10.9.0.0/16",
		"off":                        "subnet routes withdrawn",
	} {
		t.Run(args, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			fakeTailscaleOnPATH(t, "Running")
			code, stdout, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--subnet", args}) })
			if code != 0 || !strings.Contains(stdout, want) {
				t.Errorf("--subnet %s: exit %d stdout %q stderr %q, want 0 and %q", args, code, stdout, stderr, want)
			}
		})
	}
}
