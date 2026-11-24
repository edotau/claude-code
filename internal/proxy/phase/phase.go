// Package phase registers the in-process phase proxy host; only the binary links it, so no test package cycles.
package phase

import (
	"context"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxy/server"
	"github.com/edotau/claude-code/internal/proxyteam"
)

func init() { agent.HostPhaseProxy = host }

func host(ctx context.Context, id string) (string, func()) {
	cfg, _ := proxyteam.Load(proxy.TeamOverlay())
	t := cfg.TeamFor(proxyteam.KindGroup)
	if t.Host != proxyteam.HostPhase {
		return "", func() {}
	}
	s, err := server.Open(ctx, proxy.Scope{Team: t.Name, ID: id})
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: phase proxy %s unavailable, dispatches use the shared proxy: %v\n", id, err)
		return "", func() {}
	}
	return id, s.Close
}
