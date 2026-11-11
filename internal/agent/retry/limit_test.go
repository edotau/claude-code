package retry

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/inflight"
)

// Verify-2: 13 batch dispatches plus 5 Agent spawns on one account never exceed the shared cap of 12, every dispatch and
// spawn finishes, and the marker dir is empty at the end. The limiter's caps lift the in-process DefaultPerKey=4
// that runTasks uses, so the file-backed shared limiter is the binding gate.
func TestBatchAndSpawnsShareTheAccountCap(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const account = "shared·acct"
	const capA = 12
	lim := inflight.Limits{Base: capA, Total: 1000}

	helper := exec.Command("sleep", "30")
	if err := helper.Start(); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	owner := helper.Process.Pid
	t.Cleanup(func() { _ = helper.Process.Kill(); _ = helper.Wait() })

	l := newLimiter(13, capA)
	l.limits = lim

	stop := make(chan struct{})
	var active, peak atomic.Int32 // holders of a slot right now: batch dispatches plus spawns
	hold := func(d time.Duration) {
		n := active.Add(1)
		for cur := peak.Load(); n > cur && !peak.CompareAndSwap(cur, n); cur = peak.Load() {
		}
		time.Sleep(d)
		active.Add(-1)
	}

	var wg sync.WaitGroup
	for range 13 { // 13 batch dispatches: exactly what one dispatch does in l.acquire — take, hold, release
		wg.Go(func() {
			release, err := l.acquire(context.Background(), account)
			if err != nil {
				t.Errorf("dispatch acquire: %v", err)
				return
			}
			hold(150 * time.Millisecond)
			release()
		})
	}
	for i := range 5 { // 5 Agent spawns from a second, live owner pid; retry a refused claim
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			holdSpawnClaim(t, i, owner, account, lim, stop, hold)
		}(i)
	}
	wg.Wait()
	close(stop)

	assertAccountPeakAndMarkers(t, account, peak.Load(), capA)
}

func holdSpawnClaim(t *testing.T, i, owner int, account string, lim inflight.Limits, stop <-chan struct{}, hold func(time.Duration)) {
	t.Helper()
	id := fmt.Sprintf("spawn-%d", i)
	for {
		ok, _ := inflight.TryClaim(inflight.Claim{Owner: owner, ID: id, Account: account, N: 1}, lim)
		if ok {
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-stop:
			t.Errorf("spawn %d never claimed", i)
			return
		}
	}
	hold(200 * time.Millisecond)
	inflight.Release(owner, id)
}

func assertAccountPeakAndMarkers(t *testing.T, account string, peak int32, capA int) {
	t.Helper()
	if got := peak; got > int32(capA) {
		t.Errorf("peak concurrent holders on %s = %d, want <= %d", account, got, capA)
	}
	if n := hosttest.CountFiles(inflight.Dir(), inflight.Prefix, account); n != 0 {
		t.Errorf("%d markers for %s remain after everything released, want 0", n, account)
	}
	if entries, _ := os.ReadDir(inflight.Dir()); len(entries) > 0 {
		names := []string{} // .lock is not a marker and may stay
		for _, e := range entries {
			if _, ok := inflight.Owner(e.Name()); ok {
				names = append(names, e.Name())
			}
		}
		if len(names) > 0 {
			t.Errorf("marker dir not empty at the end: %v", names)
		}
	}
}

// A dispatch waiting on a full shared account holds no local worker: with Jobs=1 a dispatch on a free account still runs,
// and a cancelled wait (or a dispatch cancelled while capacity is free) leaves no marker behind.
func TestSharedWaitHoldsNoLocalWorker(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const full, free = "full·acct", "free·acct"
	lim := inflight.Limits{Base: 1, Total: 1000}
	if ok, _ := inflight.TryClaim(inflight.Claim{Owner: os.Getpid(), ID: "other", Account: full}, lim); !ok {
		t.Fatal("seed claim refused")
	}
	l := newLimiter(1, DefaultPerKey)
	l.limits = lim
	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	tried := make(chan struct{}, 1)
	observeRefusedClaim(t, full, tried)
	go func() { _, err := l.acquire(ctx, full); waited <- err }()
	select {
	case <-tried: // the full-account dispatch was refused and is now polling
	case <-time.After(5 * time.Second):
		t.Fatal("full-account dispatch never tried the shared claim")
	}

	assertFreeAccountRuns(t, l, free)
	cancel()
	if err := <-waited; err == nil {
		t.Fatal("cancelled wait returned a permit")
	}
	if _, err := l.acquire(ctx, free); err == nil {
		t.Fatal("an already-cancelled dispatch got a permit while capacity was free")
	}
	if n := hosttest.CountFiles(inflight.Dir(), inflight.Prefix, full) + hosttest.CountFiles(inflight.Dir(), inflight.Prefix, free); n != 1 {
		t.Errorf("markers = %d, want only the seed claim", n)
	}
}

func observeRefusedClaim(t *testing.T, full string, tried chan<- struct{}) {
	t.Helper()
	origClaim := sharedTryClaim
	sharedTryClaim = func(c inflight.Claim, lim inflight.Limits) (bool, inflight.Snapshot) {
		ok, err := origClaim(c, lim)
		if c.Account == full && !ok {
			select {
			case tried <- struct{}{}:
			default:
			}
		}
		return ok, err
	}
	t.Cleanup(func() { sharedTryClaim = origClaim })
}

func assertFreeAccountRuns(t *testing.T, l *Limiter, free string) {
	t.Helper()
	got := make(chan error, 1)
	go func() {
		release, err := l.acquire(context.Background(), free)
		if err == nil {
			release()
		}
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("free-account dispatch: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a dispatch on a free account starved behind one waiting on a full account (Jobs=1)")
	}
}
