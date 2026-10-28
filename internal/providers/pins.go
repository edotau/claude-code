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

// Pin keys: vault only, never the process env. A model pin may be "provider:model" to cross providers.
const (
	PinProfile = "CLAUDE_PROFILE" // the session's profile
	PinModel   = "CLAUDE_MODEL"   // fills every slot
	// PinCopilotProfile names the copilot (github) profile, whose login field is the GitHub account; unset = gh's active.
	PinCopilotProfile = "COPILOT_PROFILE"
	// PinCopilotEffort is copilot's --reasoning-effort on every launch; unset follows copilot's own default.
	PinCopilotEffort = "COPILOT_EFFORT"
	// PinCopilotContext is copilot's --context tier on every launch; unset follows copilot's own default.
	PinCopilotContext = "COPILOT_CONTEXT"
	// PinCopilotModel is the copilot agent's own model, independent of the github provider's opus slot
	// (which opencode's fallback ladder also reads); unset falls back to that slot.
	PinCopilotModel = "COPILOT_MODEL"
	// PinCodexProfile, PinGeminiProfile and PinOpencodeProfile name each agent's profile; unset = the agent's own.
	PinCodexProfile    = "CODEX_PROFILE"
	PinGeminiProfile   = "GEMINI_PROFILE"
	PinOpencodeProfile = "OPENCODE_PROFILE"
	// PinCodexModel is codex's own model (its `debug models` catalog); unset follows codex's own default
	// (config.toml's model, else its ChatGPT-login provider's opus slot).
	PinCodexModel = "CODEX_MODEL"
	// PinCodexEffort is codex's -c model_reasoning_effort on every launch; unset follows codex's own default.
	PinCodexEffort = "CODEX_EFFORT"
	// PinGeminiModel is agy's own model (its `models` catalog, effort baked into the id); unset falls
	// back to agent.AgyDefaultModel.
	PinGeminiModel = "GEMINI_MODEL"
)

// isSlotPin reports whether key is CLAUDE_<TIER>_MODEL for a known tier.
func isSlotPin(key string) bool {
	return slices.ContainsFunc(models.Slots, func(slot string) bool { return key == SlotPin(slot) })
}

// SlotPin is the per-slot pin key, e.g. CLAUDE_OPUS_MODEL; it beats PinModel.
func SlotPin(slot string) string { return "CLAUDE_" + strings.ToUpper(slot) + "_MODEL" }

// EnvSessionProfile is the env var a launcher stamps into a session's child env so its statusline and hooks
// know the session's profile. It is a session marker, not a pin: the vault pin of the same name is separate.
const EnvSessionProfile = PinProfile

// SessionProfile is the profile of the session this process runs in ("" outside a launched session).
func SessionProfile() string { return strings.TrimSpace(os.Getenv(EnvSessionProfile)) }

// CopilotModel is the one resolver every copilot agent launch, the configure page and doctor share:
// PinCopilotModel first, else the github provider's opus slot (no pin = no behavior change).
func CopilotModel(reg *Registry) (model, source string) {
	if m := Pin(PinCopilotModel); m != "" {
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

// CopilotLogin resolves PinCopilotProfile: the pinned github profile's Login ("" when it has none), and that profile.
// Unset, or a pin naming no github profile, yields ("", "") = gh's active account.
func CopilotLogin(reg *Registry) (login, profile string) {
	pin := Pin(PinCopilotProfile)
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

// CodexModel resolves CODEX_MODEL: the pin if set, else "" — codex's own default (config.toml's
// model, then its ChatGPT-login provider's opus slot) decides.
func CodexModel() (model, source string) {
	if m := Pin(PinCodexModel); m != "" {
		return m, "pin"
	}
	return "", "default"
}

// CodexEffort resolves CODEX_EFFORT: the pin if set, else "" — codex's own default effort.
func CodexEffort() (effort, source string) {
	if e := Pin(PinCodexEffort); e != "" {
		return e, "pin"
	}
	return "", "default"
}

// GeminiModel resolves GEMINI_MODEL: the pin if set, else "" — agy's own default (AgyDefaultModel) decides.
func GeminiModel() (model, source string) {
	if m := Pin(PinGeminiModel); m != "" {
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
	pin, _ := pinLookup()
	return pin(key)
}

// pinLookup reads the vault pins once.
func pinLookup() (func(string) string, error) {
	pins, err := readPins()
	return func(key string) string { return strings.TrimSpace(pins[key]) }, err
}

// readPins is the vault pins table in one read transaction; a missing vault is empty.
func readPins() (pins map[string]string, err error) {
	err = vault.View(func(tx *vault.Tx) error { pins = tx.All(vault.Pins); return nil })
	return pins, err
}

// SetPins writes (value != "") or clears (value == "") keys in the vault pins in one transaction.
func SetPins(kv map[string]string) error {
	if err := vault.Update(func(tx *vault.Tx) error {
		for k, v := range kv {
			if err := tx.Put(vault.Pins, k, v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	AuditPins(kv)
	return nil
}

// AuditPins records pin changes after their transaction succeeds.
func AuditPins(kv map[string]string) {
	for k, v := range kv {
		switch {
		case k == PinProfile:
			audit.Log(audit.ProviderSwitch, v, pinCaller())
		case k == PinModel || isSlotPin(k):
			audit.Log(audit.ModelPin, k, cmp.Or(v, "unpinned"))
		}
	}
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

// RenderPins renders vault pins in the legacy format for history.
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
