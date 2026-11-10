package agent

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// Settings is one launch's resolved agent settings: the merged record under the request's flags.
type Settings struct {
	Agent       string
	Record      vault.AgentRecord // agentconfig.Settings(Agent)
	Effort      string            // request (flag or routed cell) > Record.Effort; "" = the CLI's default
	Permissions Permissions       // ResolvePermissions(record policy, request)
	Target      providers.Target  // the resolved profile row and model; Provider nil on the agent's own login

	Tools, ReadOnly, Autonomous, SafeMode bool
	Headless                              bool // false for an interactive launch
	WorkDir                               string
}

// ResolveSettings merges agentName's record with req for t; on a bad record policy Settings still carries the record.
func ResolveSettings(agentName string, req RunOptions, t providers.Target, headless bool) (Settings, error) {
	rec, err := agentconfig.Settings(agentName)
	if err != nil {
		return Settings{}, err
	}
	s := Settings{Agent: agentName, Record: rec, Effort: cmp.Or(req.Effort, rec.Effort), Permissions: req.Permissions,
		Target: t, Tools: req.Tools, ReadOnly: req.ReadOnly, Autonomous: req.Autonomous, SafeMode: req.SafeMode,
		Headless: headless, WorkDir: req.WorkDir}
	def, err := policyPermissions(rec.Permissions)
	if err != nil {
		return s, fmt.Errorf("%s default permissions: %w", agentName, err)
	}
	s.Permissions, _ = ResolvePermissions(def, req.Permissions)
	return s, nil
}

// policyPermissions parses a record policy; nil is no policy.
func policyPermissions(p *vault.AgentPolicy) (Permissions, error) {
	if p == nil {
		return Permissions{}, nil
	}
	return ParsePermissions(p.Mode, p.Allow, p.Ask, p.Deny)
}

// recordFieldOwners are the record fields only one agent can use; every other agent refuses them.
var recordFieldOwners = []struct {
	field, owner string
	set          func(vault.AgentRecord) bool
}{
	{"context", agentid.Copilot, func(r vault.AgentRecord) bool { return r.Context != "" }},
	{"account", agentid.Copilot, func(r vault.AgentRecord) bool { return r.Account != "" }},
	{"aliases", agentid.Claude, func(r vault.AgentRecord) bool { return len(r.Aliases) > 0 }},
}

// ValidateRecord checks a record for agentName: effort, policy grammar, extras keys and launch env names.
func ValidateRecord(agentName string, r vault.AgentRecord) error {
	if !agentid.Known(agentName) {
		return fmt.Errorf("unknown agent %q (%s)", agentName, strings.Join(agentid.All(), "|"))
	}
	if err := agentid.ValidEffort(agentName, r.Effort); err != nil {
		return err
	}
	for _, f := range recordFieldOwners {
		if f.set(r) && f.owner != agentName {
			return fmt.Errorf("%s: %s is %s only", agentName, f.field, f.owner)
		}
	}
	if _, err := policyPermissions(r.Permissions); err != nil {
		return fmt.Errorf("%s permissions: %w", agentName, err)
	}
	if err := checkExtras(agentName, SchemaFor(agentName).CLI.Extras, r.Extras); err != nil {
		return err
	}
	if r.Launch != nil {
		if _, err := agentconfig.FlagOverlay(r.Launch.Env, nil); err != nil {
			return fmt.Errorf("%s launch.env: %w", agentName, err)
		}
	}
	return nil
}

// checkExtras refuses an extras key decl lacks and a value outside its allowed list; "" (unset a built-in) passes.
func checkExtras(agentName string, decl map[string][]string, extras map[string]string) error {
	for _, k := range slices.Sorted(maps.Keys(extras)) {
		allowed, ok := decl[k]
		if !ok {
			want := cmp.Or(strings.Join(slices.Sorted(maps.Keys(decl)), ", "), "none")
			return fmt.Errorf("%s: extras key %q unsupported (want %s)", agentName, k, want)
		}
		if v := extras[k]; v != "" && allowed != nil && !slices.Contains(allowed, v) {
			return fmt.Errorf("%s: extras.%s %q: want one of %s", agentName, k, v, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// EffortClamp is the Emulated note for an effort agentName's CLI clamps ("effort max→xhigh"); "" when none.
func EffortClamp(agentName, effort string) string {
	native, clamped := agentid.NativeEffort(agentName, agentid.Effort(effort))
	if !clamped {
		return ""
	}
	return "effort " + effort + "→" + native
}
