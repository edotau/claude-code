package agentconfig

import (
	"context"
	"errors"
	"fmt"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

// cloudLanes maps each agent with a vendor cloud lane to its provider key and the vendor's name.
var cloudLanes = map[string][2]string{"claude": {"anthropic", "Anthropic"}, "codex": {"openai", "OpenAI"}}

// CheckCloudAgent refuses cloud on an agent that has no vendor cloud lane.
func (o Overlay) CheckCloudAgent(agent string) error {
	if _, ok := cloudLanes[agent]; o.Cloud && !ok {
		return fmt.Errorf("overlay cloud: %s has no cloud lane (claude, codex)", agent)
	}
	return nil
}

// CloudGate allows a vendor cloud lane only on a top-level run whose overlay sets cloud on the vendor's own login.
func CloudGate(ctx context.Context, o Overlay, agent, lane, sent string) error {
	switch {
	case !o.Cloud:
		return fmt.Errorf("%s sends %s to %s's cloud: relaunch with overlay {\"cloud\":true}", lane, sent, cloudLanes[agent][1])
	case proc.InDispatch(ctx): // an outer dispatch's confinement stays; only a top-level run opts in
		return errors.New(lane + ": overlay cloud is refused inside a dispatch; set it on the outermost ask")
	}
	return OwnLogin(o, agent, lane)
}

// OwnLogin refuses an overlay that routes a cloud lane off the vendor's own login; the lane never uses a stored pin.
func OwnLogin(o Overlay, agent, lane string) error {
	vendor := cloudLanes[agent][0]
	off := fmt.Errorf("%s: runs on %s's own login only; drop the overlay provider/profile", lane, agent)
	if o.Provider != "" && o.Provider != vendor {
		return off
	}
	if o.Profile == "" || ownsProfile(agent, o.Profile) {
		return nil
	}
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	if p := reg.Providers[o.Profile]; p == nil || p.Provider != vendor {
		return off
	}
	return nil
}
