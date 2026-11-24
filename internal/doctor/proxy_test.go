package doctor

import (
	"testing"

	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxyteam"
)

func TestScopeOverReserved(t *testing.T) {
	cfg := proxyteam.Config{Teams: []proxyteam.Team{
		{Name: "d", Host: proxyteam.HostDetached, Reserved: 2},
		{Name: "a", Host: proxyteam.HostAsk, Reserved: 5},
	}}
	for _, c := range []struct {
		team, id string
		reserved int
		over     bool
	}{
		{"d", "0", 2, false}, {"d", "1", 2, false}, {"d", "2", 2, true},
		{"a", "0", 0, true}, {"gone", "0", 0, true},
	} {
		n, over := scopeOverReserved(proxy.Scope{Team: c.team, ID: c.id}, cfg)
		if n != c.reserved || over != c.over {
			t.Errorf("%s@%s = (%d, %v), want (%d, %v)", c.team, c.id, n, over, c.reserved, c.over)
		}
	}
}
