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
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// Settings is one launch's resolved agent settings: the merged record under the request's flags.
type Settings struct {
	Agent       string
	Record      vault.AgentRecord // agentconfig.Settings(Agent)
	Effort      string            // request (flag or routed cell) > Record.Effort; "" = the CLI's default
	Permissions Permissions       // resolvePermissions(record policy, request)
	Target      providers.Target  // the resolved profile row and model; Provider nil on the agent's own login

	Tools, Autonomous, SafeMode bool
	Headless                    bool // false for an interactive launch
	WorkDir                     string
}

// ResolveSettings merges agentName's checked record with req; a bad one errs with built-in Settings (none: Agent "").
func ResolveSettings(agentName string, req RunOptions, t providers.Target, headless bool) (Settings, error) {
	rec, err := agentconfig.Settings(agentName)
	if err == nil {
		err = ValidateRecord(agentName, rec)
	}
	if err == nil {
		return newSettings(agentName, rec, req, t, headless), nil
	}
	base, berr := agentconfig.BuiltinSettings(agentName) // unchecked, as every launch applied it before records
	if berr != nil {
		return Settings{}, err
	}
	return newSettings(agentName, base, req, t, headless), err
}

// newSettings is Settings for rec; an unparsable record policy layers as none (ValidateRecord reports it).
func newSettings(agentName string, rec vault.AgentRecord, req RunOptions, t providers.Target, headless bool) Settings {
	def, _ := policyPermissions(rec.Permissions)
	perms, _ := resolvePermissions(def, req.Permissions)
	return Settings{Agent: agentName, Record: rec, Effort: cmp.Or(req.Effort, rec.Effort), Permissions: perms,
		Target: t, Tools: req.Tools, Autonomous: req.Autonomous, SafeMode: req.SafeMode,
		Headless: headless, WorkDir: req.WorkDir}
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

// ValidateRecord checks agentName's record: effort, owners, policy, declared extras, launch env and args (unechoed).
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
	if decl := SchemaFor(agentName).CLI.Extras; decl != nil { // nil: the vendor has not declared its keys yet
		if err := checkExtras(agentName, decl, r.Extras); err != nil {
			return err
		}
	}
	return checkLaunch(agentName, r.Launch)
}

// checkLaunch refuses launch.env names --env refuses and a credential-shaped launch.args entry, by index.
func checkLaunch(agentName string, l *vault.AgentLaunch) error {
	if l == nil {
		return nil
	}
	if _, err := agentconfig.FlagOverlay(l.Env, nil); err != nil {
		return fmt.Errorf("%s launch.env: %w", agentName, err)
	}
	if i := slices.IndexFunc(l.Args, secretArg); i >= 0 {
		return fmt.Errorf("%s launch.args[%d]: looks like a credential: store it with `claude-code vault set` or a "+
			"provider's auth.command", agentName, i)
	}
	return nil
}

// secretArg reports a credential-shaped argv entry, whole or as the value of --flag=value.
func secretArg(a string) bool {
	_, v, _ := strings.Cut(a, "=")
	return textutil.LooksSecret(a) || textutil.LooksSecret(v)
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
