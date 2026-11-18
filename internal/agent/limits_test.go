package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

const limitRoot = "20000101T000000.000-root"
const limitParent = "20000101T000001.000-parent"
const limitUnrelatedTokens = 999
const limitInheritedTokens = 500000
const limitBenchmarkRecords = 200

func seedLimitRecord(t testing.TB, rec Record) {
	t.Helper()
	if err := paths.WriteJSON(filepath.Join(paths.DispatchDir(), rec.ID+".json"), rec); err != nil {
		t.Fatal(err)
	}
}

func assertLimitAsk(t *testing.T, values proc.Values, want string, job *Record) {
	t.Helper()
	before, err := recordNames(paths.DispatchDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = buildAskRecord(t.Context(), "codex", RunOptions{Values: values}, job, t.TempDir(), false)
	if want == "" {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	assertLimitRefusal(t, err, want)
	after, err := recordNames(paths.DispatchDir())
	if err != nil || len(after) != len(before) {
		t.Fatalf("refusal wrote record: %v, %v", after, err)
	}
}

func assertLimitRefusal(t *testing.T, err error, want string) {
	t.Helper()
	var refusal *retry.Error
	if !errors.As(err, &refusal) || refusal.Kind != retry.KindFatal || !refusal.Terminal || refusal.Agent != "codex" || refusal.Err.Error() != want {
		t.Fatalf("error = %v; want terminal fatal %q", err, want)
	}
	t.Log(err)
}

func TestBuildAskRecordWidthCap(t *testing.T) {
	for _, tc := range []struct {
		name, max         string
		live              int
		top, job, refused bool
	}{
		{name: "at cap", live: 8, refused: true},
		{name: "below cap", live: 7},
		{name: "garbage", max: "garbage", live: 8, refused: true},
		{name: "unlimited", max: "0", live: 8},
		{name: "top level", max: "1", live: 8, top: true},
		{name: "resume", max: "1", live: 8, job: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setConfigDir(t)
			values := proc.Values{maxDispatchChildrenEnv: tc.max, maxTreeTokensEnv: "0", proc.DispatchIDEnv: limitParent, proc.DispatchDepthEnv: "0"}
			if tc.top {
				values = values.With(proc.DispatchIDEnv, "")
			}
			for i := range tc.live {
				seedLimitRecord(t, Record{ID: fmt.Sprintf("child-%02d", i), Parent: limitParent, Status: "running", Time: time.Now()})
			}
			seedLimitRecord(t, Record{ID: "finished", Parent: limitParent, Status: "ok"})
			seedLimitRecord(t, Record{ID: "unrelated", Parent: "other", Status: "running", Time: time.Now()})
			want := ""
			if tc.refused {
				max := tc.live
				want = fmt.Sprintf("dispatch width %d reached for parent %s (%d running); wait for one to finish or set HARNESS_DISPATCH_MAX_CHILDREN (0 = no cap)", max, limitParent, tc.live)
			}
			var job *Record
			if tc.job {
				job = &Record{ID: "resumed", Parent: limitParent, Depth: 1}
			}
			assertLimitAsk(t, values, want, job)
		})
	}
}

func TestBuildAskRecordTreeBudget(t *testing.T) {
	for _, tc := range []struct {
		name, max         string
		top, job, refused bool
	}{
		{name: "at cap", max: "20", refused: true},
		{name: "below cap", max: "21"},
		{name: "default"},
		{name: "top level", max: "1", top: true},
		{name: "resume", max: "1", job: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setConfigDir(t)
			seedLimitRecord(t, Record{ID: limitRoot, Status: "ok", Usage: &wire.Usage{InputTokens: 5}})
			seedLimitRecord(t, Record{ID: limitParent, Parent: limitRoot, Status: "ok"})
			seedLimitRecord(t, Record{ID: "20000101T000002.000-sibling", Parent: limitRoot, Usage: &wire.Usage{OutputTokens: 7}})
			seedLimitRecord(t, Record{ID: "20000101T000003.000-grandchild", Parent: limitParent, Usage: &wire.Usage{CacheReadTokens: 3, CacheCreationTokens: 5}})
			seedLimitRecord(t, Record{ID: "20000101T000004.000-unrelated", Usage: &wire.Usage{InputTokens: limitUnrelatedTokens}})
			values := proc.Values{maxDispatchChildrenEnv: "0", maxTreeTokensEnv: tc.max, proc.DispatchIDEnv: limitParent, proc.DispatchDepthEnv: "0"}
			if tc.top {
				values = values.With(proc.DispatchIDEnv, "")
			}
			want := ""
			if tc.refused {
				want = fmt.Sprintf("dispatch tree budget 20 tokens spent under root %s (20 counted; 1 records without usage); set HARNESS_DISPATCH_MAX_TREE_TOKENS (0 = no cap)", limitRoot)
			}
			var job *Record
			if tc.job {
				job = &Record{ID: "resumed", Parent: limitParent, Depth: 1}
			}
			assertLimitAsk(t, values, want, job)
		})
	}
}

func TestDispatchLimitEnvironmentInheritance(t *testing.T) {
	t.Setenv(maxDispatchChildrenEnv, "6")
	t.Setenv(maxTreeTokensEnv, "500000")
	ctx := proc.WithValues(t.Context(), proc.ValuesFromEnv((EnvSpec{}).Environ(t.Context())))
	if got := dispatchLimit(ctx, maxDispatchChildrenEnv, 8); got != 6 {
		t.Fatalf("children = %d", got)
	}
	if got := dispatchLimit(ctx, maxTreeTokensEnv, 0); got != limitInheritedTokens {
		t.Fatalf("tokens = %d", got)
	}
}

func BenchmarkLiveDispatchChildren200(b *testing.B) {
	dir := b.TempDir()
	for i := range limitBenchmarkRecords {
		rec := Record{ID: fmt.Sprintf("child-%03d", i), Parent: limitParent, Status: "running", Time: time.Now()}
		body, err := rec.encode()
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rec.ID+".json"), body, 0o600); err != nil {
			b.Fatal(err)
		}
	}
	for b.Loop() {
		if n := liveDispatchChildren(dir, limitParent); n != limitBenchmarkRecords {
			b.Fatalf("children = %d", n)
		}
	}
}

func TestDispatchWidthLockTimeoutFailsOpen(t *testing.T) {
	setConfigDir(t)
	wait := 20 * time.Millisecond
	old := dispatchChildrenLockWait
	dispatchChildrenLockWait = wait
	t.Cleanup(func() { dispatchChildrenLockWait = old })
	values := proc.Values{maxDispatchChildrenEnv: "1", maxTreeTokensEnv: "0", proc.DispatchIDEnv: limitParent, proc.DispatchDepthEnv: "0"}
	seedLimitRecord(t, Record{ID: "child", Parent: limitParent, Status: "running", Time: time.Now()})
	lock := filepath.Join(paths.DispatchDir(), ".children")
	err := paths.WithFileLockWithin(lock, wait, func() error {
		start := time.Now()
		assertLimitAsk(t, values, "", nil)
		if elapsed := time.Since(start); elapsed < wait {
			t.Fatalf("lock timeout took %v", elapsed)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDispatchWidthConcurrentClaims(t *testing.T) {
	setConfigDir(t)
	ctx := proc.WithValues(t.Context(), proc.Values{maxDispatchChildrenEnv: "3", maxTreeTokensEnv: "0"})
	results := make(chan error, 6)
	for i := range 6 {
		go func() {
			rec := Record{ID: fmt.Sprintf("concurrent-%d", i), Parent: limitParent, Status: "running", Time: time.Now()}
			_, err := claimLimitedRecord(ctx, "codex", rec)
			results <- err
		}()
	}
	admitted := 0
	for range 6 {
		if err := <-results; err == nil {
			admitted++
		} else {
			var refusal *retry.Error
			if !errors.As(err, &refusal) || refusal.Kind != retry.KindFatal {
				t.Fatal(err)
			}
		}
	}
	if got := liveDispatchChildren(paths.DispatchDir(), limitParent); got != 3 || admitted != 3 {
		t.Fatalf("live children = %d, admitted = %d; want 3", got, admitted)
	}
}

func TestDispatchLimitOverlayPrecedence(t *testing.T) {
	for _, key := range []string{maxDispatchChildrenEnv, maxTreeTokensEnv} {
		for _, tc := range []struct {
			env, doc string
			want     int
		}{
			{"7", `{"maxChildren":2,"maxTreeTokens":2}`, 7},
			{"", `{"maxChildren":2,"maxTreeTokens":2}`, 2},
			{"", `{"maxChildren":0,"maxTreeTokens":0}`, 0},
			{"", `{"maxChildren":-1,"maxTreeTokens":"bad"}`, 8},
			{"bad", `{"maxChildren":2,"maxTreeTokens":2}`, 8},
			{"", "{}", 8},
		} {
			ctx := proc.WithValues(t.Context(), proc.Values{key: tc.env, "COWORKER_OVERLAY": tc.doc})
			if got := dispatchLimit(ctx, key, 8); got != tc.want {
				t.Errorf("%s %+v: got %d", key, tc, got)
			}
		}
	}
}

func TestClaimLimitedRecordCreatesPrivateDispatchDir(t *testing.T) {
	setConfigDir(t)
	ctx := proc.WithValues(t.Context(), proc.Values{maxDispatchChildrenEnv: "3", maxTreeTokensEnv: "0"})
	if _, err := claimLimitedRecord(ctx, "codex", Record{ID: "private-1", Parent: limitParent, Status: "running", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(paths.DispatchDir()); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700) {
		t.Errorf("dispatch dir = %v, %v; want 0700", fi, err)
	}
}
