package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
)

// countAccountMarkers counts live marker files in dir whose content is account.
func countAccountMarkers(dir, account string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if _, ok := inflight.Owner(e.Name()); !ok {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && strings.TrimSpace(string(b)) == account {
			n++
		}
	}
	return n
}

// samplePeak counts account markers every interval until stop closes and returns the peak.
func samplePeak(dir, account string, interval time.Duration, stop <-chan struct{}, peak *atomic.Int32) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		n := int32(countAccountMarkers(dir, account))
		for cur := peak.Load(); n > cur && !peak.CompareAndSwap(cur, n); cur = peak.Load() {
		}
		time.Sleep(interval)
	}
}

// Verify-2: 13 batch legs plus 5 Agent spawns on one account never exceed the shared cap of 12, every leg and
// spawn finishes, and the marker dir is empty at the end. The limiter's caps lift the in-process DefaultPerKey=4
// that RunBatch uses, so the file-backed shared limiter is the binding gate.
func TestBatchAndSpawnsShareTheAccountCap(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const account = "shared·acct"
	const capA = 12
	lim := inflight.Limits{Base: capA, Total: 1000, Available: []string{account}}

	helper := exec.Command("sleep", "30")
	if err := helper.Start(); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	owner := helper.Process.Pid
	t.Cleanup(func() { _ = helper.Process.Kill(); _ = helper.Wait() })

	l := newLimiter(13, capA)
	l.limits = lim

	stop := make(chan struct{})
	var active, peak atomic.Int32 // holders of a slot right now: batch legs plus spawns
	hold := func(d time.Duration) {
		n := active.Add(1)
		for cur := peak.Load(); n > cur && !peak.CompareAndSwap(cur, n); cur = peak.Load() {
		}
		time.Sleep(d)
		active.Add(-1)
	}

	var wg sync.WaitGroup
	for range 13 { // 13 batch legs: exactly what one leg does in l.acquire — take, hold, release
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.acquire(context.Background(), account)
			if err != nil {
				t.Errorf("leg acquire: %v", err)
				return
			}
			hold(150 * time.Millisecond)
			release()
		}()
	}
	for i := range 5 { // 5 Agent spawns from a second, live owner pid; retry a refused claim
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
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
		}(i)
	}
	wg.Wait()
	close(stop)

	if got := peak.Load(); got > capA {
		t.Errorf("peak concurrent holders on %s = %d, want <= %d", account, got, capA)
	}
	if n := countAccountMarkers(inflight.Dir(), account); n != 0 {
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

// RunBatch writes one shared marker per running leg and removes every marker when the batch is done.
func TestRunBatchMarkersAppearAndClear(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const account = "shared·acct"
	var cur, runPeak atomic.Int32
	var tasks []BatchTask
	for _, name := range []string{"a", "b", "c"} {
		r := gaugeRunner{name: name, cur: &cur, max: &runPeak}
		tasks = append(tasks, BatchTask{ID: name, Agent: name, Req: Request{Prompt: "p"},
			Prepare: func(context.Context) (Chain, Record, error) {
				return Chain{Want: name, Legs: []Leg{{Agent: r, Account: account, keepCaller: true}}},
					Record{ID: "id-" + name, Agent: name, Log: filepath.Join(t.TempDir(), "l.log")}, nil
			}})
	}
	stop := make(chan struct{})
	var peak atomic.Int32
	go samplePeak(inflight.Dir(), account, 2*time.Millisecond, stop, &peak)

	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: 3}) {
		t.Fatal("batch failed")
	}
	close(stop)
	if got := peak.Load(); got == 0 {
		t.Error("no inflight markers observed while the batch ran")
	}
	if n := countAccountMarkers(inflight.Dir(), account); n != 0 {
		t.Errorf("%d markers remain after RunBatch returned, want 0", n)
	}
}
