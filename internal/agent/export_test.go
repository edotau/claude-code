package agent

import (
	"github.com/edotau/claude-code/internal/router"
)

// SetChain and KeepCaller let the agent_test batch tests build a Prepared with a routed chain.
func SetChain(p *Prepared, c Chain) *Prepared { p.chain = c; return p }

func KeepCaller(d Dispatch) Dispatch { d.keepCaller = true; return d }

// Hooks for the batch runner tests, which live in agent_test because batch imports agent.
var (
	CoolDown            = coolDown
	AuthEnv             = authEnv
	CountAccountMarkers = countAccountMarkers
	SamplePeak          = samplePeak
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
