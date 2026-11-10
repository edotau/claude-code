package agent

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/router"
)

// KeepCaller lets the agent_test batch tests build a dispatch that keeps the caller's own --model.
func KeepCaller(d Dispatch) Dispatch { d.keepCaller = true; return d }

// Hooks for the batch runner tests, which live in agent_test because batch imports agent.
var (
	AuthEnv = authEnv
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

// fastDefaultBackoff shrinks DefaultPolicy's backoff for the test and restores it on cleanup.
func fastDefaultBackoff(t *testing.T) {
	t.Helper()
	b, m := retry.DefaultBaseDelay, retry.DefaultMaxDelay
	retry.DefaultBaseDelay, retry.DefaultMaxDelay = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { retry.DefaultBaseDelay, retry.DefaultMaxDelay = b, m })
}
