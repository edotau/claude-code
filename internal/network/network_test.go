package network

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

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agent.StubProxyForTest("http://127.0.0.1:9999", "rtr-secret-test")
	agentconfig.SetCatalogWarn(io.Discard)
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	gateway.HolderComm = func(int) string { return "claude-code" }
	code := m.Run()
	cleanup()
	hosttest.Cleanup()
	os.Exit(code)
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	_, stdout, _ := hosttest.CaptureOutput(t, func() int { fn(); return 0 })
	return stdout
}

func TestNetworkBareAndHelpPrintUsageExitZero(t *testing.T) {
	for _, args := range [][]string{nil, {"-h"}, {"--help"}} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run(args) })
		if code != 0 {
			t.Errorf("network %v: exit %d, want 0", args, code)
		}
		for _, sub := range []string{"tailscale", "browser", "websearch", "restart", "remote"} {
			if !strings.Contains(stderr, sub) {
				t.Errorf("network %v: help lacks %q: %q", args, sub, stderr)
			}
		}
	}
}

func TestNetworkUnknownSubverb(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"bogus"}) })
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, `unknown subcommand "bogus"`) || !strings.Contains(stderr,
		"tailscale|browser|websearch|restart|remote") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestNetworkOldSubverbsRejected(t *testing.T) {
	for _, old := range []string{"web", "doctor", "status", "connect", "browsers"} {
		if code, _, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{old}) }); code != 1 {
			t.Errorf("network %s: exit %d, want 1", old, code)
		}
	}
}

// isolateRouterState points the router state dir at a temp dir so no real daemon is ever seen.
func isolateRouterState(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
}

func decodeRestart(t *testing.T, out string) gateway.RestartRow {
	t.Helper()
	var doc struct {
		Daemons []gateway.RestartRow `json:"daemons"`
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
		if row := decodeRestart(t, out); row.Daemon != "proxy" || row.Status != "skipped" || row.OldPID != 0 ||
			row.NewPID != 0 {
			t.Errorf("%v: row = %+v", args, row)
		}
		if _, err := os.Stat(filepath.Join(paths.StateDir(), "router.json")); err == nil {
			t.Errorf("%v: a skipped restart created router.json", args)
		}
	}
}

func TestRestartDryRunRunningRouterChangesNothing(t *testing.T) {
	isolateRouterState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		_ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
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
