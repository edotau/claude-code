package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proxy"
)

func restartRows(t *testing.T, args ...string) (int, []proxy.RestartRow) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, _ := hosttest.CaptureOutput(t, func() int {
		return cmdGatewayProxy(append([]string{"--restart", "--format", "json"}, args...))
	})
	var doc struct {
		Daemons []proxy.RestartRow `json:"daemons"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json = %q (%v)", out, err)
	}
	return code, doc.Daemons
}

func TestRestartReportStoppedProxyIsSkipped(t *testing.T) {
	for _, flag := range []string{"--if-changed", "--dry-run"} {
		code, rows := restartRows(t, flag)
		if code != 0 || len(rows) != 1 || rows[0].Daemon != "proxy" || rows[0].Status != "skipped" {
			t.Errorf("%s: exit %d rows %+v", flag, code, rows)
		}
		if _, err := os.Stat(filepath.Join(paths.StateDir(), "proxy.json")); err == nil {
			t.Errorf("%s: a skipped restart created proxy.json", flag)
		}
	}
}

type restartTransport func(*http.Request) (*http.Response, error)

func (f restartTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRestartReportUnchangedKeepsTunnels(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	oldClient := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = oldClient })
	http.DefaultClient = &http.Client{Transport: restartTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{proxy.PIDHeader: {strconv.Itoa(os.Getpid())}, proxy.HeaderInflight: {"1"}},
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})}
	if err := os.MkdirAll(paths.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	cleanup, err := proxy.WriteState(proxy.State{PID: os.Getpid(), Port: 23456, Exe: proxy.ExeHash()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	dir := filepath.Join(paths.StateDir(), "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tunnel := `{"host":"test-host","port":12345,"pid":1073741824}`
	if err := os.WriteFile(filepath.Join(dir, "egress-test.json"), []byte(tunnel), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := hosttest.CaptureOutput(t, func() int {
		return cmdGatewayProxy([]string{"--restart", "--if-changed", "--dry-run", "--format", "json"})
	})
	if code != 0 || !strings.Contains(out, `"unchanged"`) || strings.Contains(out, "egress") {
		t.Fatalf("exit %d: unchanged proxy must not schedule a tunnel stop: %s", code, out)
	}
}

func TestRestartFlagsNeedRestart(t *testing.T) {
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdGatewayProxy([]string{"--if-changed"}) }); code == 0 {
		t.Fatal("--if-changed without --restart accepted")
	}
}

func TestRestartReportStoppedProxyKeepsTunnels(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := filepath.Join(paths.StateDir(), "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tunnel := `{"host":"test-host","port":12345,"pid":1073741824}`
	if err := os.WriteFile(filepath.Join(dir, "egress-test.json"), []byte(tunnel), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := hosttest.CaptureOutput(t, func() int {
		return cmdGatewayProxy([]string{"--restart", "--if-changed", "--dry-run", "--format", "json"})
	})
	if code != 0 || strings.Contains(out, "egress") {
		t.Fatalf("exit %d: a skipped restart must not stop tunnels: %s", code, out)
	}
}
