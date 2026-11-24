package ask

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/router"
)

func TestMemoryRunOptionsNeverFallBack(t *testing.T) {
	o := memoryRunOptions(context.Background(), "p", "/w")
	if o.Fallback != router.FallbackNone || o.Agent != "claude" || !o.Tools || o.WorkDir != "/w" {
		t.Fatalf("memoryRunOptions = %+v, want claude with tools and FallbackNone", o)
	}
}

// An ask brief gains one recall block, keeps a held one unchanged, and a retry replays the sent bytes.
func TestAskAttachesRecallOnce(t *testing.T) {
	dir := t.TempDir()
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "HOME", "PATH"} {
		t.Setenv(k, dir)
	}
	task := "stage the deploy pipeline staging signoff rollout " + strings.Repeat("and verify the result ", 10)
	run := func(args ...string) string {
		hosttest.CaptureOutput(t, func() int {
			return Cmd(append([]string{"--agent", "claude", "--tools", "--fallback", "none", "--format", "json"}, args...))
		})
		return agent.RecentRecords(1)[0].Request.Prompt
	}
	sent := task + "\n\n" + agent.ReviewLoopRule + "\n"
	if bare := run(task); bare != sent {
		t.Fatalf("empty bank: prompt = %q, want the task plus the review rule", bare)
	}
	bareID := agent.RecentRecords(1)[0].ID
	var b strings.Builder
	for i := range 3 {
		fmt.Fprintf(&b, "- deploy pipeline staging signoff rule %d <!-- id:%08x -->\n", i, i+1)
	}
	bank := filepath.Join(paths.MemoryStoreDir(), "conventions.md")
	if err := os.MkdirAll(filepath.Dir(bank), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bank, []byte("# Conventions\n\n"+b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	first := run(task)
	if strings.Count(first, "<memory-recall>") != 1 || !strings.HasPrefix(first, task) {
		t.Fatalf("prompt = %q, want the task plus one block", first)
	}
	id := agent.RecentRecords(1)[0].ID
	if held := run(first); held != first {
		t.Errorf("a brief holding a block changed: %q", held)
	}
	if again := run("--retry", id); again != first {
		t.Errorf("retry sent %q, want %q", again, first)
	}
	if again := run("--retry", bareID); again != sent {
		t.Errorf("retry of a bare send = %q, want %q", again, sent)
	}
}

// A catchable kill cancels the memory run's context, and the receipt names the signal.
func TestMemorySignalsCancelAndNameTheKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGHUP delivery on windows")
	}
	ctx, stop := memorySignals(t.Context())
	self, _ := os.FindProcess(os.Getpid())
	if err := self.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not cancel the memory run")
	}
	sig := stop()
	if sig != syscall.SIGHUP {
		t.Fatalf("stop() = %v, want SIGHUP", sig)
	}
	got := memoryReceipt(7, "", 1500*time.Millisecond, sig, ctx.Err())
	if want := "memory job: pid 7 dispatch - ended after 1.5s: error: context canceled (signal hangup)"; got != want {
		t.Errorf("receipt = %q, want %q", got, want)
	}
	if _, stop := memorySignals(t.Context()); stop() != nil {
		t.Error("stop() with no signal named one")
	}
}
