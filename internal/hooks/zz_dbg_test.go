package hooks

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/preflight"
	"github.com/edotau/claude-code/internal/providers"
)

func TestZZDbg(t *testing.T) {
	servedSession(t, true, time.Time{}, "")
	t.Logf("on=%v", providers.ServedRoutingOn())
	reg, err := providers.Load()
	t.Logf("err=%v prov=%v", err, reg.Providers)
	ps := preflight.Check(reg, time.Now())
	t.Logf("pins=%+v", ps)
	t.Logf("profiles=%v stale=%v", preflight.Profiles(reg, ps), preflight.Stale(reg, time.Now()))
}
