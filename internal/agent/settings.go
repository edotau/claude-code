package agent

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// Settings is one launch's resolved agent settings: the merged record under the request's flags.
type Settings struct {
	Values      proc.Values `json:"-"`
	LaunchEnv   proc.Values `json:"-"`
	LaunchArgs  []string
	Agent       string
	Record      vault.AgentRecord // agentconfig.Settings(Agent)
	Effort      string            // request (flag or routed cell) > Record.Effort; "" = the CLI's default
	Permissions Permissions       // resolvePermissions(record policy, request)
	Target      providers.Target  // the resolved profile row and model; Provider nil on the agent's own login

	Tools, Autonomous, SafeMode bool
	Gate                        bool // claude: the permission-gate hook enforces Permissions, so no permission flags render
	Headless                    bool // false for an interactive launch
	WorkDir                     string
}

// ResolveSettings merges agentName's checked record with req; a bad one errs with built-in Settings (none: Agent "").
func ResolveSettings(agentName string, req RunOptions, t providers.Target, headless bool) (Settings, error) {
	rec, err := agentconfig.Settings(agentName)
	if err == nil {
		var st Settings
		st, err = SettingsFromRecord(agentName, rec, req, t, headless)
		if err == nil {
			return st, nil
		}
	}
	base, berr := agentconfig.BuiltinSettings(agentName)
	if berr != nil {
		return Settings{}, err
	}
	// The legacy overlay is part of base and was applied unchecked before stored records existed.
	return newSettings(agentName, base, req, t, headless), err
}

// SettingsFromRecord validates a stored record and copies its resolved runtime settings without resolving secrets.
func SettingsFromRecord(agentName string, rec vault.AgentRecord, req RunOptions, t providers.Target, headless bool) (Settings, error) {
	st, err := validateRecord(agentName, rec)
	if err != nil {
		return Settings{}, err
	}
	out := newSettings(agentName, rec, req, t, headless)
	out.LaunchEnv = st.LaunchEnv
	return out, nil
}

// newSettings copies runtime fields without revalidating the legacy fallback.
func newSettings(agentName string, rec vault.AgentRecord, req RunOptions, t providers.Target, headless bool) Settings {
	def, _ := policyPermissions(rec.Permissions) // An unparsable fallback policy layers as none, as before.
	st := Settings{Permissions: def}
	rec = rec.Clone()
	st.Agent, st.Record, st.Target = agentName, rec, t
	st.Effort = cmp.Or(req.Effort, rec.Effort)
	st.Permissions, _ = resolvePermissions(st.Permissions, req.Permissions)
	st.Tools, st.Autonomous, st.SafeMode, st.Gate = req.Tools, req.Autonomous, req.SafeMode, req.Gate
	st.Headless, st.WorkDir = headless, req.WorkDir
	if req.Values != nil {
		st.Values = req.Values.Merge(nil)
	}
	if rec.Launch != nil {
		st.LaunchEnv = proc.ValuesFromEnv(rec.Launch.Env)
		st.LaunchArgs = slices.Clone(rec.Launch.Args)
	}
	if t.Provider != nil {
		p := *t.Provider
		p.Headers, p.Env, p.Models = maps.Clone(p.Headers), maps.Clone(p.Env), maps.Clone(p.Models)
		p.Routes = maps.Clone(p.Routes)
		st.Target.Provider = &p
	}
	return st
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
	_, err := validateRecord(agentName, r)
	return err
}

func validateRecord(agentName string, r vault.AgentRecord) (Settings, error) {
	var st Settings
	if !agentid.Known(agentName) {
		return st, fmt.Errorf("unknown agent %q (%s)", agentName, strings.Join(agentid.All(), "|"))
	}
	if err := agentid.ValidEffort(agentName, r.Effort); err != nil {
		return st, err
	}
	for _, f := range recordFieldOwners {
		if f.set(r) && f.owner != agentName {
			return st, fmt.Errorf("%s: %s is %s only", agentName, f.field, f.owner)
		}
	}
	var err error
	if st.Permissions, err = policyPermissions(r.Permissions); err != nil {
		return st, fmt.Errorf("%s permissions: %w", agentName, err)
	}
	if decl := SchemaFor(agentName).CLI.Extras; decl != nil {
		if err := checkExtras(agentName, decl, r.Extras); err != nil {
			return st, err
		}
	}
	st.LaunchEnv, err = checkLaunchValues(agentName, r.Launch)
	return st, err
}

// checkLaunchValues validates launch env and args without resolving references or echoing credentials.
func checkLaunchValues(agentName string, l *vault.AgentLaunch) (proc.Values, error) {
	if l == nil {
		return nil, nil
	}
	overlay, err := agentconfig.FlagOverlay(l.Env, nil)
	if err != nil {
		return nil, fmt.Errorf("%s launch.env: %w", agentName, err)
	}
	if i := slices.IndexFunc(l.Args, secretArg); i >= 0 {
		return nil, fmt.Errorf("%s launch.args[%d]: looks like a credential: store it with `claude-code vault set` or a "+
			"provider's auth.command", agentName, i)
	}
	return proc.Values(overlay.Set).Merge(nil), nil
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
