package agentcmd

import (
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func captureFailedStdout(t *testing.T, run func() int) (int, string) {
	t.Helper()
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if os.IsNotExist(err) {
		t.Skip("requires /dev/full")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := full.Close(); err != nil {
			t.Error(err)
		}
	}()
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		original := os.Stdout
		os.Stdout = full
		defer func() { os.Stdout = original }()
		return run()
	})
	return code, stderr
}

func requireOutputFailure(t *testing.T, code int, stderr, diagnostic string) {
	t.Helper()
	if code != 1 || !strings.Contains(stderr, diagnostic) || !strings.Contains(stderr, "no space left on device") {
		t.Fatalf("lost output: exit %d, stderr %q; want exit 1 with %q and write error", code, stderr, diagnostic)
	}
}

func TestAgentsResultReportsTextWriteFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)
	rec.Log = rec.LogPath()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	run := func() int { return Job("result", []string{rec.ID}) }
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if code != 0 || !strings.Contains(stdout, "hi there") {
		t.Fatalf("result control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stderr = captureFailedStdout(t, run)
	requireOutputFailure(t, code, stderr, "agents result: write output:")
}

func TestAgentsTreeReportsJSONWriteFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	run := func() int { return Run([]string{"tree", "--format", "json"}) }
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if code != 0 || !json.Valid([]byte(stdout)) {
		t.Fatalf("tree control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stderr = captureFailedStdout(t, run)
	requireOutputFailure(t, code, stderr, "agents tree:")
}

func TestAnthropicPassesSubcommandHelp(t *testing.T) {
	vaulttest.UseFreshKey(t)
	pathstest.FakeVendor(t, "claude", `printf 'vendor argv: %s %s\n' "$1" "$2"`)
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return Anthropic([]string{"mcp", "--help"})
	})
	if code != 0 || !strings.Contains(stdout, "vendor argv: mcp --help") {
		t.Fatalf("subcommand help: exit %d, stdout %q, stderr %q; want vendor MCP help", code, stdout, stderr)
	}
	code, stdout, stderr = hosttest.CaptureOutput(t, func() int { return Anthropic([]string{"--help"}) })
	if code != 0 || !strings.Contains(stdout, "--oauth") || strings.Contains(stdout, "vendor argv:") {
		t.Fatalf("wrapper help control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestSplitReviewReportsMergedWriteFailure(t *testing.T) {
	vaulttest.UseFreshKey(t)
	vaulttest.SetRecord(t, "opencode", "profile", "zen-free")
	t.Setenv("OPENCODE_API_KEY", "test-key")
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"SHIP IT"}}'`)
	parent := job.NewRecord("opencode", agent.RunOptions{Prompt: "review"}, time.Now())
	if err := parent.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(reviewJobEnv, parent.ID)
	run := func() int {
		return runSplitReview("opencode", nil, []roles.Part{{Diff: []byte("test diff")}},
			roles.Scope{Kind: "uncommitted"}, false, false, "", "", "")
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if code != 0 || !strings.Contains(stdout, "SHIP IT") {
		t.Fatalf("review control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stderr = captureFailedStdout(t, run)
	requireOutputFailure(t, code, stderr, "review: write merged report:")
	got, err := job.LoadRecord(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || !strings.Contains(got.Error, "write merged report:") || got.Progress != "" {
		t.Fatalf("lost merged review marked successful: %+v", got)
	}
}

func serveSuccessfulLiveTurn(t *testing.T, listener net.Listener) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			done <- err
			return
		}
		var request wire.LiveRequest
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			done <- err
			return
		}
		env := wire.NewEnvelope(wire.Result{Agent: "claude", Answer: "turn complete"}, nil, "")
		done <- json.NewEncoder(conn).Encode(wire.Event{Type: wire.EventResult, Envelope: &env})
	}()
	return done
}

func liveSendFixture(t *testing.T) (job.Record, net.Listener) {
	t.Helper()
	dir, err := os.MkdirTemp("", "send-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	rec := job.NewRecord("claude", agent.RunOptions{Prompt: "live"}, time.Now())
	rec.ID, rec.Live, rec.PID = "send-test", true, os.Getpid()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", job.LiveSocket(rec.ID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	return rec, listener
}

func TestLiveSendReportsJSONWriteFailure(t *testing.T) {
	rec, listener := liveSendFixture(t)
	run := func() int { return Send([]string{rec.ID, "hello", "--format", "json"}) }
	done := serveSuccessfulLiveTurn(t, listener)
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if code != 0 || !strings.Contains(stdout, "turn complete") || !json.Valid([]byte(stdout)) {
		t.Fatalf("send control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	done = serveSuccessfulLiveTurn(t, listener)
	code, stderr = captureFailedStdout(t, run)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requireOutputFailure(t, code, stderr, "agents send: write output:")
}
