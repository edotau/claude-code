package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/router"
)

// SetChain and KeepCaller let the agent_test batch tests build a Prepared with a routed chain.
func SetChain(p *Prepared, c Chain) *Prepared { p.chain = c; return p }

func KeepCaller(d Dispatch) Dispatch { d.keepCaller = true; return d }

// Hooks for the batch runner tests, which live in agent_test because batch imports agent.
var (
	CoolDown = retry.CoolDown
	AuthEnv  = authEnv
)

// SwapLoadState replaces the router state loader with wrap(prev) and returns the restore func.
func SwapLoadState(wrap func(prev func() router.State) func() router.State) func() {
	prev := loadState
	loadState = wrap(prev)
	return func() { loadState = prev }
}

// NewFailingRunner is a fakeRunner whose first attempt fails with err and whose answer is empty.
func NewFailingRunner(name string, err error) Engine {
	return &fakeRunner{name: name, errs: []error{err}, emptyAnswer: true}
}

// CountAccountMarkers counts live marker files in dir whose content is account.
func CountAccountMarkers(dir, account string) int {
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

// SamplePeak counts account markers every interval until stop closes and returns the peak.
func SamplePeak(dir, account string, interval time.Duration, stop <-chan struct{}, peak *atomic.Int32) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		n := int32(CountAccountMarkers(dir, account))
		for cur := peak.Load(); n > cur && !peak.CompareAndSwap(cur, n); cur = peak.Load() {
		}
		time.Sleep(interval)
	}
}

// fastDefaultBackoff shrinks DefaultPolicy's backoff for the test and restores it on cleanup.
func fastDefaultBackoff(t *testing.T) {
	t.Helper()
	b, m := retry.DefaultBaseDelay, retry.DefaultMaxDelay
	retry.DefaultBaseDelay, retry.DefaultMaxDelay = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { retry.DefaultBaseDelay, retry.DefaultMaxDelay = b, m })
}
