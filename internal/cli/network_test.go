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
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
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
