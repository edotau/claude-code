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

// Pin keys: process env beats vault pins. A model pin may be "provider:model" to cross providers.
const (
	PinProvider = "HARNESS_PROVIDER"
	PinModel    = "HARNESS_MODEL" // fills every slot
	// PinGitHubUser names the gh account copilot runs as (`gh auth token --user`); unset = gh's active one.
	PinGitHubUser = "COPILOT_GITHUB_USER"
	// PinCopilotEffort is copilot's --reasoning-effort on every launch; unset follows copilot's own default.
	PinCopilotEffort = "COPILOT_REASONING_EFFORT"
	// PinCopilotContext is copilot's --context tier on every launch; unset follows copilot's own default.
	PinCopilotContext = "COPILOT_CONTEXT_TIER"
	// PinCopilotModel is the copilot agent's own model, independent of the github-copilot provider's
	// opus slot (which opencode's fallback ladder also reads); unset falls back to that slot.
	PinCopilotModel = "COPILOT_MODEL"
	// PinCodexModel is codex's own model (its `debug models` catalog); unset follows codex's own default
	// (config.toml's model, else its ChatGPT-login provider's opus slot).
	PinCodexModel = "CODEX_MODEL"
	// PinCodexEffort is codex's -c model_reasoning_effort on every launch; unset follows codex's own default.
	PinCodexEffort = "CODEX_REASONING_EFFORT"
	// PinGeminiModel is agy's own model (its `models` catalog, effort baked into the id); unset falls
	// back to agent.AgyDefaultModel.
	PinGeminiModel = "GEMINI_AGENT_MODEL"
)

// SlotPin is the per-slot pin key, e.g. HARNESS_OPUS_MODEL; it beats PinModel.
func SlotPin(slot string) string { return "HARNESS_" + strings.ToUpper(slot) + "_MODEL" }

// CopilotModel is the one resolver every copilot agent launch, the configure page and doctor share:
// PinCopilotModel first, else github-copilot's opus slot (no pin = no behavior change).
func CopilotModel(reg *Registry) (model, source string) {
	if m := Pin(PinCopilotModel); m != "" {
		return m, "pin"
	}
	if p, ok := reg.Providers["github-copilot"]; ok {
		return p.Models[models.Opus], "github-copilot default"
	}
	return "", "github-copilot default"
}

// CodexModel resolves CODEX_MODEL: the pin if set, else "" — codex's own default (config.toml's
// model, then its ChatGPT-login provider's opus slot) decides.
func CodexModel() (model, source string) {
	if m := Pin(PinCodexModel); m != "" {
		return m, "pin"
	}
	return "", "default"
}

// CodexEffort resolves CODEX_REASONING_EFFORT: the pin if set, else "" — codex's own default effort.
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

// Pin reads key from the process env, then the vault pins.
func Pin(key string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	pin, _ := pinLookup()
	return pin(key)
}

// pinLookup reads the vault pins once; process env takes precedence even when the vault is unreadable.
func pinLookup() (func(string) string, error) {
	pins, err := readPins()
	return func(key string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return pins[key]
	}, err
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
		case k == PinProvider:
			audit.Log(audit.ProviderSwitch, v, pinCaller())
		case k == PinModel || (strings.HasPrefix(k, "HARNESS_") && strings.HasSuffix(k, "_MODEL")):
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
		if cur[k] == kv[k] {
			continue
		}
		out = append(out, k+": "+cmp.Or(cur[k], "(unset)")+" → "+cmp.Or(kv[k], "(unset)"))
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
