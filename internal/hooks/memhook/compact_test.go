package memhook

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
)

func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// compactSession is a hermetic banked session with work worth one update; it returns its PreCompact input.
func compactSession(t *testing.T) (in *core.Input, handoffs string) {
	t.Helper()
	swap(t, &stewardWait, 50*time.Millisecond)
	tr, payload := hookSession(t)
	hosttest.WriteEdits(t, tr, 3)
	in = payload()
	if err := os.MkdirAll(filepath.Dir(memory.Path("activeContext.md")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memory.Path("activeContext.md"), []byte("# A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return in, memory.SubDir("handoffs")
}

// cancellable is a hook context the test ends itself, so a blocked half never holds the test for the budget.
func cancellable(t *testing.T) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	swap(t, &compactContext, func() (context.Context, context.CancelFunc) { return ctx, cancel })
	return cancel
}

// blocked returns a func that waits until the test ends: a half that overruns its budget.
func blocked(t *testing.T) func() {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func() { <-release }
}

func generated(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "*-compact-s1.md"))
	return m
}

func TestCompactWritesHandoffWhileUpdateBlocks(t *testing.T) {
	in, dir := compactSession(t)
	cancel := cancellable(t)
	wait := blocked(t)
	swap(t, &compactUpdate, func(*core.Input, time.Time) { wait() })
	swap(t, &compactHandoff, func(ctx context.Context, in *core.Input, now time.Time) {
		writeGeneratedHandoff(ctx, in, now)
		cancel()
	})
	if r := Compact(in); !reflect.DeepEqual(r, core.Result{}) {
		t.Fatalf("Compact = %+v, want proceed", r)
	}
	if got := generated(t, dir); len(got) != 1 {
		t.Fatalf("generated handoffs = %v, want one despite the blocked update", got)
	}
}

func TestCompactDispatchesUpdateWhileHandoffBlocks(t *testing.T) {
	in, dir := compactSession(t)
	spawns := memory.StubSpawn(t, true)
	cancel := cancellable(t)
	wait := blocked(t)
	swap(t, &compactHandoff, func(context.Context, *core.Input, time.Time) { wait() })
	swap(t, &compactUpdate, func(in *core.Input, now time.Time) {
		updateBeforeCompact(in, now)
		cancel()
	})
	Compact(in)
	if got := spawns.Update.Load(); got != 1 || len(generated(t, dir)) != 0 {
		t.Fatalf("updates = %d, want 1 despite the blocked handoff", got)
	}
}

// An unwritable handoffs home costs only the handoff: the hook proceeds and the update still dispatches.
func TestCompactProceedsWhenHandoffsHomeUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions as a non-root user")
	}
	in, dir := compactSession(t)
	spawns := memory.StubSpawn(t, true)
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if r := Compact(in); !reflect.DeepEqual(r, core.Result{}) || spawns.Update.Load() != 1 {
		t.Fatalf("Compact = %+v, updates = %d; want proceed and one update", r, spawns.Update.Load())
	}
	if got := generated(t, dir); len(got) != 0 {
		t.Fatalf("generated = %v in an unwritable dir", got)
	}
}

// The generated handoff lists only this session's dispatches: running with their owned files, finished counted.
func TestGeneratedHandoffListsTheSessionsDispatches(t *testing.T) {
	in, dir := compactSession(t)
	now := time.Now()
	swap(t, &sessionOwner, func() int { return 4242 })
	swap(t, &recentRecords, func(int) []agent.Record {
		return []agent.Record{
			{ID: "r", Agent: "claude", Model: "opus", Status: "running", PID: os.Getpid(), Owner: 4242, Time: now.Add(-5 * time.Minute),
				Request: agent.RunOptions{Prompt: "TASK ID: t-run\nOwns: /abs/a.go"}},
			{ID: "d", Agent: "codex", Status: "ok", Owner: 4242, Time: now.Add(-time.Minute), Request: agent.RunOptions{Prompt: "TASK ID: t-done"}},
			{ID: "f", Agent: "codex", Status: "running", PID: os.Getpid(), Owner: 99, Time: now, Request: agent.RunOptions{Prompt: "TASK ID: foreign"}},
		}
	})
	writeGeneratedHandoff(context.Background(), in, now)
	files := generated(t, dir)
	if len(files) != 1 {
		t.Fatalf("generated = %v", files)
	}
	data, _ := os.ReadFile(files[0])
	got := string(data)
	for _, want := range []string{"- t-run: claude · opus · 5m · Owns: /abs/a.go", "(1): t-done (ok)"} {
		if !strings.Contains(got, want) {
			t.Errorf("handoff lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "foreign") {
		t.Errorf("another session's dispatch listed:\n%s", got)
	}
}
