package ask

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
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

func TestMemoryHopsEndWithLocal(t *testing.T) {
	ws := func(n string) *providers.Provider {
		return &providers.Provider{Name: n, Provider: providers.GroupDatabricks}
	}
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close()
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"dev": ws("dev"), "qa": ws("qa"), "prod": ws("prod"),
		"local": {Name: "local", Provider: "magnitude", BaseURL: url, Models: map[string]string{"sonnet": "q"}},
	}}
	got := memoryHops(reg, "dev")
	if len(got) != 3 || got[2] != localRoute || got[0].Profile == "dev" || got[1].Profile == "dev" {
		t.Fatalf("hops for dev = %v, want the other two workspaces then local", got)
	}
	if got := memoryHops(reg, "work"); len(got) != 1 || got[0] != (route.Route{Provider: "magnitude", Profile: "local"}) {
		t.Fatalf("hops for a non-Databricks pin = %v, want [local]", got)
	}
	start := time.Now()
	if localUp(context.Background(), reg) || time.Since(start) > localProbe+time.Second {
		t.Fatalf("localUp against a closed port = true or slow (%s)", time.Since(start))
	}
	checkJobClasses(t, reg)
}

func checkJobClasses(t *testing.T, reg *providers.Registry) {
	t.Helper()
	for _, tc := range []struct {
		err       error
		class, fx string
	}{
		{providers.ErrNeedsLogin, memory.JobClassAuth, "claude-code databricks login --profile dev"},
		{fmt.Errorf("x: %w", agent.ErrTreeBudget), memory.JobClassBudget, "claude-code memory update --catchup"},
		{errors.New("boom"), memory.JobClassFailed, ""},
	} {
		if c, fix := jobClass(reg, "dev", tc.err); c != tc.class || fix != tc.fx {
			t.Fatalf("jobClass(%v) = %q %q, want %q %q", tc.err, c, fix, tc.class, tc.fx)
		}
	}
}
