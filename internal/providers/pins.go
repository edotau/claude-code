package providers

import (
	"bufio"
	"cmp"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/vault"
)

// EnvSessionProfile is the env var a launcher stamps into a session's child env so its statusline and hooks
// know the session's profile.
const EnvSessionProfile = "CLAUDE_PROFILE"

// SessionProfile is the profile of the session this process runs in ("" outside a launched session).
func SessionProfile() string { return strings.TrimSpace(os.Getenv(EnvSessionProfile)) }

// agentRecords reads every stored agent record in one vault transaction.
func agentRecords() (recs map[string]vault.AgentRecord, err error) {
	err = vault.View(func(tx *vault.Tx) (e error) { recs, e = tx.AgentRecords(); return e })
	return recs, err
}

// AgentRecords is every stored agent record from one vault read (for packages that cannot import internal/agent).
func AgentRecords() map[string]vault.AgentRecord {
	recs, _ := agentRecords()
	return recs
}

// AgentRecord is agent's vault record (zero value when unset or unreadable).
func AgentRecord(agent string) vault.AgentRecord { return AgentRecords()[agent] }

// CopilotModel is the one resolver every copilot agent launch, the configure page and doctor share:
// the record's model first, else the github provider's opus slot (no pin = no behavior change).
func CopilotModel(reg *Registry) (model, source string) {
	if m := AgentRecord("copilot").Model; m != "" {
		return m, "pin"
	}
	if reg == nil {
		return "", "github-copilot default"
	}
	if names := reg.ProfilesOf("github"); len(names) > 0 {
		return reg.Providers[names[0]].Models[models.Opus], "github-copilot default"
	}
	return "", "github-copilot default"
}

// CopilotLogin resolves the copilot record's profile: that github profile's Login ("" when it has none), and the profile.
// Off, or a profile naming no github profile, yields ("", "") = gh's active account.
func CopilotLogin(reg *Registry) (login, profile string) {
	pin := AgentRecord("copilot").Profile
	if pin == "" {
		return "", ""
	}
	if reg != nil {
		if p := reg.Providers[pin]; p != nil && p.Provider == "github" {
			return p.Login, pin
		}
	}
	return "", ""
}

// GithubAccountName is the route-account name (github·<name>) for a gh login: its profile, else the login itself.
func GithubAccountName(reg *Registry, login string) string {
	if reg != nil && login != "" {
		for _, n := range reg.ProfilesOf("github") {
			if reg.Providers[n].Login == login {
				return n
			}
		}
	}
	return login
}

// GithubAccountLogin is the gh login behind a route-account name: the profile's Login, else name as a raw login.
func GithubAccountLogin(reg *Registry, name string) string {
	if reg != nil {
		if p := reg.Providers[name]; p != nil && p.Provider == "github" {
			return p.Login
		}
	}
	return name
}

// CodexModel resolves the codex record's model: it if set, else "" — codex's own default (config.toml's
// model, then its ChatGPT-login provider's opus slot) decides.
func CodexModel() (model, source string) {
	if m := AgentRecord("codex").Model; m != "" {
		return m, "pin"
	}
	return "", "default"
}

// CodexEffort resolves the codex record's effort: it if set, else "" — codex's own default effort.
func CodexEffort() (effort, source string) {
	if e := AgentRecord("codex").Effort; e != "" {
		return e, "pin"
	}
	return "", "default"
}

// GeminiModel resolves the gemini record's model: it if set, else "" — agy's own default (AgyDefaultModel) decides.
func GeminiModel() (model, source string) {
	if m := AgentRecord("gemini").Model; m != "" {
		return m, "pin"
	}
	return "", "default"
}

// ReadEnvFile parses KEY=VALUE lines (# comments, optional export, optional quotes); a missing file is empty.
func ReadEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// Pin reads key from the vault pins; it never reads the process env.
func Pin(key string) string {
	pin, _ := PinLookup()
	return pin(key)
}

// PinLookup reads the vault pins once.
func PinLookup() (func(string) string, error) {
	pins, err := readPins()
	return func(key string) string { return strings.TrimSpace(pins[key]) }, err
}

// readPins is the vault pins table (non-agent keys; agent settings live in records).
func readPins() (pins map[string]string, err error) {
	err = vault.View(func(tx *vault.Tx) error { pins = tx.All(vault.Pins); return nil })
	return pins, err
}

// SetPins writes (value != "") or clears (value == "") keys of the pins table in one transaction.
func SetPins(kv map[string]string) error {
	return vault.Update(func(tx *vault.Tx) error {
		for k, v := range kv {
			if err := tx.Put(vault.Pins, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// AuditRecords audits claude's record changes after their transaction succeeds: a profile switch, a model pin or unpin.
func AuditRecords(before, after map[string]vault.AgentRecord) {
	b, n := recordFields(before["claude"]), recordFields(after["claude"])
	for _, f := range slices.Sorted(maps.Keys(mergeKeys(b, n))) {
		switch {
		case b[f] == n[f]:
		case f == "profile":
			audit.Log(audit.ProviderSwitch, n[f], pinCaller())
		case f == "model" || strings.HasPrefix(f, "slots."):
			audit.Log(audit.ModelPin, "claude."+f, cmp.Or(n[f], "unpinned"))
		}
	}
}

// AuditSwitch records a session-profile switch.
func AuditSwitch(profile string) { audit.Log(audit.ProviderSwitch, profile, pinCaller()) }

// AuditModelPin records a claude model pin for slot ("" = every slot); an empty spec is an unpin.
func AuditModelPin(slot, spec string) {
	key := "claude.model"
	if slot != "" {
		key = "claude.slots." + slot
	}
	audit.Log(audit.ModelPin, key, cmp.Or(spec, "unpinned"))
}

// RecordDiff lists "agent.field: old → new" for every record field that differs between before and after, sorted.
func RecordDiff(before, after map[string]vault.AgentRecord) []string {
	var out []string
	for _, a := range slices.Sorted(maps.Keys(mergeKeys(before, after))) {
		b, n := recordFields(before[a]), recordFields(after[a])
		for _, f := range slices.Sorted(maps.Keys(mergeKeys(b, n))) {
			if b[f] != n[f] {
				out = append(out, a+"."+f+": "+cmp.Or(b[f], "(unset)")+" → "+cmp.Or(n[f], "(unset)"))
			}
		}
	}
	return out
}

func mergeKeys[V any](a, b map[string]V) map[string]struct{} {
	keys := map[string]struct{}{}
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	return keys
}

// recordFields flattens r to field -> value, tier slots as slots.<tier>.
func recordFields(r vault.AgentRecord) map[string]string {
	f := map[string]string{"profile": r.Profile, "model": r.Model, "effort": r.Effort, "context": r.Context, "account": r.Account}
	for tier, v := range r.Slots {
		f["slots."+tier] = v
	}
	return f
}

// PinDiff previews SetPins(kv): one "KEY: old → new" line per key whose value would change, sorted.
func PinDiff(kv map[string]string) ([]string, error) {
	cur, err := readPins()
	if err != nil {
		return nil, err
	}
	keys := slices.Sorted(maps.Keys(kv))
	var out []string
	for _, k := range keys {
		was := strings.TrimSpace(cur[k])
		if was == kv[k] {
			continue
		}
		out = append(out, k+": "+cmp.Or(was, "(unset)")+" → "+cmp.Or(kv[k], "(unset)"))
	}
	return out, nil
}

// RenderPins renders the vault pins table as the plain-text snapshot history records.
func RenderPins() ([]byte, error) {
	pins, err := readPins()
	if err != nil {
		return nil, err
	}
	if len(pins) == 0 {
		return nil, fs.ErrNotExist
	}
	keys := slices.Sorted(maps.Keys(pins))
	var b strings.Builder
	b.WriteString("# Managed by `claude-code switch` / `claude-code models --pin`.\n")
	for _, k := range keys {
		b.WriteString(k + "=" + pins[k] + "\n")
	}
	return []byte(b.String()), nil
}

// OAuthTokenEnv is the env var Claude Code reads a long-lived Max-plan token from (`claude setup-token` mints one).
const OAuthTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

// HeadlessOAuthToken reads the setup-token from the process env, then the vault.
func HeadlessOAuthToken() string {
	if tok := strings.TrimSpace(os.Getenv(OAuthTokenEnv)); tok != "" {
		return tok
	}
	return StoredSecret(OAuthTokenEnv)
}

// StoredSecret reads a vault secret; a miss or read error yields an empty string.
func StoredSecret(name string) string {
	v, _, err := vault.ReadSecret(name)
	if err != nil {
		return ""
	}
	return v
}

// EffectiveSecret reads the process env, then the vault, preserving vault read errors.
func EffectiveSecret(name string) (value, source string, err error) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, "env", nil
	}
	v, found, err := vault.ReadSecret(name)
	if err != nil {
		return "", "", err
	}
	if found {
		return v, "vault", nil
	}
	return "", "", nil
}

// HasSecret reports env or vault presence without resolving a master key; a vault read error reads as absent.
func HasSecret(name string) string {
	source, _ := HasSecretErr(name)
	return source
}

// HasSecretErr is HasSecret with the vault read error surfaced, for callers (configure provider --list) that must
// not mistake an unreadable vault for "no credential".
func HasSecretErr(name string) (source string, err error) {
	if strings.TrimSpace(os.Getenv(name)) != "" {
		return "env", nil
	}
	found := false
	err = vault.View(func(tx *vault.Tx) error {
		_, found = tx.Get(vault.Secrets, name)
		return nil
	})
	if err != nil {
		return "", err
	}
	if found {
		return "vault", nil
	}
	return "", nil
}

// pinCaller names who wrote a pin — the verb and the parent process — so an unexpected switch can be traced.
func pinCaller() string {
	verb := ""
	if len(os.Args) > 1 {
		verb = os.Args[1]
	}
	return "via " + cmp.Or(verb, "?") + " (parent " + cmp.Or(proc.Comm(os.Getppid()), "?") + ")"
}

// SpellPin is pin spelled the way p serves it, or "" when p cannot serve it (a Databricks wire id off Databricks).
func SpellPin(p *Provider, pin string) string {
	if p == nil {
		return pin
	}
	if p.Preset != PresetDatabricks && strings.HasPrefix(pin, models.DatabricksPrefix) {
		return ""
	}
	return p.WireModel(models.Strip1M(pin))
}
