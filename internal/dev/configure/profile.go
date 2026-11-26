// profile.go — ApplyProfile, the one action every profile change goes through: it sets each agent's record.
package configure

import (
	"context"
	"fmt"
	"io"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/dev/preflight"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
)

// ApplyProfile sets profile on each differing agent record in one vault update and returns the agents changed.
func ApplyProfile(agents []string, profile string) (changed []string, err error) {
	if err := ValidProfile(agents, profile); err != nil {
		return nil, err
	}
	rt := routeFor(profile)
	err = vault.UpdateAgentRecords(func(m map[string]vault.AgentRecord) {
		changed = nil
		for _, a := range agents {
			if profileKey(m[a].EffectiveRoute()) != profile {
				m[a] = WithProfile(m[a], rt, profile)
				changed = append(changed, a)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return changed, nil
}

// routeFor is the route a profile name stores: the pool for "databricks", else its registry row's provider; zero when none.
func routeFor(profile string) route.Route {
	reg, err := providers.Load()
	if err != nil {
		return route.Route{}
	}
	return RouteIn(reg, profile)
}

// RouteIn is the route profile stores in reg; zero when reg has no provider for it.
func RouteIn(reg *providers.Registry, profile string) route.Route {
	if rt := providers.CompleteRoute(reg, route.Route{Profile: profile}); rt.Provider != "" {
		return rt
	}
	return route.Route{}
}

// WithProfile stores rt as r's route; a zero rt keeps a non-empty name as an unresolved profile.
func WithProfile(r vault.AgentRecord, rt route.Route, name string) vault.AgentRecord {
	r.Route = rt
	if rt.IsZero() {
		r.Route = route.Route{Profile: name}
	}
	return r
}

// withAccount sets r's copilot account route; "" clears only an account route.
func withAccount(r vault.AgentRecord, account string) vault.AgentRecord {
	switch {
	case account != "":
		return WithProfile(r, route.Route{Provider: "github", Account: account}, "")
	case r.Route.Account != "":
		r.Route = route.Route{}
	}
	return r
}

// profileKey is the profile name a route is pinned as: "databricks" for the pool, "" for an account route.
func profileKey(rt route.Route) string {
	switch {
	case rt.Account != "":
		return ""
	case rt.Provider == providers.GroupDatabricks && rt.Profile == route.AutoArm:
		return "databricks"
	}
	return rt.Profile
}

// ValidProfile rejects a profile some agent cannot run on, before any record is written; "" (off) always passes.
func ValidProfile(agents []string, profile string) error {
	if profile == "" {
		return nil
	}
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if err := agentpkg.ValidAgentPin(reg, a, profile); err != nil {
			return err
		}
		if row := reg.Providers[profile]; row != nil && !agentpkg.VaultAgentIsClaude(a) && !agentpkg.CanServe(a, row) {
			return fmt.Errorf("%s cannot serve profile %q", a, profile)
		}
	}
	return nil
}

// SwitchTargets moves claude and fixed pins on profile's provider; off, pool, own-login and other pins stay.
func SwitchTargets(reg *providers.Registry, recs map[string]vault.AgentRecord, profile string) (move, off, kept []string) {
	target, _ := reg.Get(profile)
	for _, spec := range agentpkg.Roster() {
		rt := recs[spec.Name].EffectiveRoute()
		switch {
		case switchAlreadyOn(reg, rt, target):
			kept = append(kept, spec.Name)
		case target != nil && !spec.External():
			move = append(move, spec.Name)
		case rt.Account != "":
			kept = append(kept, spec.Name)
		case profileKey(rt) == "":
			off = append(off, spec.Name)
		case switchServes(spec.Name, target) && movesOnSwitch(reg, spec.Name, rt, target):
			move = append(move, spec.Name)
		default:
			kept = append(kept, spec.Name)
		}
	}
	return move, off, kept
}

func switchAlreadyOn(reg *providers.Registry, rt route.Route, target *providers.Provider) bool {
	return target != nil && !rt.IsZero() && providers.CompleteRoute(reg, rt) == RouteIn(reg, target.Name)
}

func switchServes(name string, target *providers.Provider) bool {
	if target == nil {
		return false
	}
	ok, _ := agentpkg.Serves(name, target, "")
	return ok
}

// movesOnSwitch is true for a fixed (not pool, not own-login) profile on target's provider.
func movesOnSwitch(reg *providers.Registry, name string, rt route.Route, target *providers.Provider) bool {
	rt = providers.CompleteRoute(reg, rt)
	if target == nil || rt.Profile == "" ||
		(rt.Provider == providers.GroupDatabricks && rt.Profile == route.AutoArm) || agentpkg.OwnLogin(reg, name, rt.Profile) {
		return false
	}
	return rt.Provider == target.Provider
}

// PinLabel is the profile an agent's record is pinned as; an account route reads as its account.
func PinLabel(rec vault.AgentRecord) string {
	rt := rec.EffectiveRoute()
	if k := profileKey(rt); k != "" {
		return k
	}
	return rt.Account
}

// SwitchRecords sets profile on every SwitchTargets mover in m (the dry-run view of SwitchProfile).
func SwitchRecords(reg *providers.Registry, m map[string]vault.AgentRecord, profile string) {
	target, err := reg.Get(profile)
	if err != nil {
		return
	}
	profile = target.Name
	move, _, _ := SwitchTargets(reg, m, profile)
	for _, name := range move {
		m[name] = WithProfile(m[name], RouteIn(reg, profile), profile)
	}
}

// SwitchProfile selects and replaces eligible fixed pins in one vault transaction, then audits it.
func SwitchProfile(reg *providers.Registry, profile string) error {
	return switchProfile(reg, profile, vault.UpdateAgentRecords)
}

// switchProfile accepts the transaction runner so tests can interleave a writer at the transaction boundary.
func switchProfile(reg *providers.Registry, profile string, update func(func(map[string]vault.AgentRecord)) error) error {
	target, err := reg.Get(profile)
	if err != nil {
		return err
	}
	profile = target.Name
	if err := update(func(recs map[string]vault.AgentRecord) {
		SwitchRecords(reg, recs, profile)
	}); err != nil {
		return fmt.Errorf("switch profile: %w", err)
	}
	providers.AuditSwitch(profile)
	return nil
}

// profileStatus is the one-line `<agent> profile` readout: the record's profile, identity and on/off state.
func profileStatus(agent string) string {
	line := agent + ": no profile set (launches use today's selection)"
	if v := profileKey(vault.GetAgentRecord(agent).EffectiveRoute()); v != "" {
		line = fmt.Sprintf("%s: profile %s", agent, v) + providers.IdentityNote(v)
	}
	if !agentpkg.VaultAgentIsClaude(agent) {
		state := agentpkg.AgentOn
		if !agentpkg.AgentActive(agent) {
			state = agentpkg.AgentOff
		} else if why := agentpkg.AgentUnbillable(agent); why != "" {
			state = agentpkg.AgentOff + ", " + why
		}
		line += " · " + state
	}
	return line
}

// KeyVerified live-probes a keyed provider; false when absent or not accepted.
func KeyVerified(p *providers.Provider) bool {
	if p == nil || !providers.CredentialPresent(p) {
		return false
	}
	status, _, _ := ProbeCached(KeyTarget{Provider: p})
	return status == providers.ProbeVerified
}

// AgentProfile is `<agent> profile`: clear the pin (own login), turn the agent off, show it, or set it.
func AgentProfile(w io.Writer, name, profile string, clear, off bool) error {
	if clear && off || (clear || off) && profile != "" {
		return fmt.Errorf("%s profile: --clear, --off and a profile name are mutually exclusive", name)
	}
	switch {
	case clear:
		return clearAgentProfile(w, name)
	case off:
		return turnAgentOff(w, name)
	case profile == "":
		fmt.Fprintln(w, profileStatus(name))
		return nil
	default:
		return setAgentProfile(context.Background(), w, name, profile)
	}
}

// clearAgentProfile resets name's pin to its own login.
func clearAgentProfile(w io.Writer, name string) error {
	if _, err := ApplyProfile([]string{name}, vault.OwnLoginProfiles[name]); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: profile cleared (own login)\n", name)
	return nil
}

// turnAgentOff clears name's pin so the agent stays off; claude is always on.
func turnAgentOff(w io.Writer, name string) error {
	if agentpkg.VaultAgentIsClaude(name) {
		return fmt.Errorf("%s is always on", name)
	}
	if err := agentpkg.ValidAgentActive(name, agentpkg.AgentOff); err != nil {
		return err
	}
	if _, err := ApplyProfile([]string{name}, ""); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: %s\n", name, agentpkg.AgentOff)
	return nil
}

// setAgentProfile pins name to profile and prints what changed; copilot on a login-less github row gets a note.
func setAgentProfile(ctx context.Context, w io.Writer, name, profile string) error {
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	if _, err := ApplyProfile([]string{name}, profile); err != nil {
		return fmt.Errorf("%w (see claude-code vault ls providers)", err)
	}
	if p := reg.Providers[profile]; name == "copilot" && p != nil && p.Provider == "github" && p.Login == "" {
		fmt.Fprintf(w, "copilot: %s has no login; billing follows copilot's own login, else a stored token, else gh's active account "+
			"(pick one in `claude-code configure agents --agent copilot`)\n", profile)
	}
	fmt.Fprintf(w, "%s: now on %s; relaunch `claude-code %s` to apply\n", name, profile, name)
	preflight.Warm(ctx, w, reg, time.Now())
	if name == "codex" {
		fmt.Fprintln(w, "plain codex: run `claude-code configure restore --agent all` to re-render config.toml")
	}
	return nil
}
