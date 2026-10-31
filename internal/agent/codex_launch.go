package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
)

var idRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ownedID namespaces the provider entry so it never overwrites a user's own block of the same name.
func ownedID(p *providers.Provider) (string, error) {
	if !idRE.MatchString(p.Name) {
		return "", fmt.Errorf("provider name %q must match %s to be passed to an agent config", p.Name, idRE)
	}
	return "harness-" + p.Name, nil
}

// codexResponsesModel swaps a model Responses cannot serve (the row's opus slot, gpt-oss) for the menu's first Responses model.
func codexResponsesModel(p *providers.Provider, model string) string {
	gpt := models.FamilyKind(model) == models.FamilyGPT && !models.ChatOnly(model)
	if p.Preset != providers.PresetDatabricks || gpt || providers.ServesResponses(p, model) {
		return model
	}
	return cmp.Or(providers.FirstResponsesModel(p), model)
}

// codexRanModel is the model codex runs for t: t.Model unless the Responses swap replaced it.
func codexRanModel(t providers.Target) string {
	if t.Provider == nil {
		return t.Model
	}
	wire := t.Provider.WireModel(models.Strip1M(t.Model))
	if swapped := codexResponsesModel(t.Provider, wire); swapped != wire {
		return swapped
	}
	return t.Model
}

// codex speaks only the Responses API (`wire_api = "chat"` is rejected by current builds).
func codex(p *providers.Provider, base, model, cred string, h *Headless) ([]string, []string, error) {
	model = codexResponsesModel(p, model)
	id, err := ownedID(p)
	if err != nil {
		return nil, nil, err
	}
	fields := [][2]string{{"name", tomlQuote(p.Name)}, {"base_url", tomlQuote(base)}, {"wire_api", tomlQuote("responses")}}
	var env []string
	if p.Preset == providers.PresetDatabricks {
		env = append(env, settings.CodexProfileEnv+"="+p.Name, settings.CodexCatalogEnv+"=")
	}
	if cred != "" {
		env = append(env, CodexKeyEnv+"="+cred)
		if p.Auth.HeaderStyle() == providers.HeaderXAPIKey {
			fields = append(fields, [2]string{"env_http_headers", tomlInline([][2]string{{tomlQuote("x-api-key"), tomlQuote(CodexKeyEnv)}})})
		} else {
			fields = append(fields, [2]string{"env_key", tomlQuote(CodexKeyEnv)})
		}
	}
	if len(p.Headers) > 0 {
		var hs [][2]string
		for _, k := range slices.Sorted(maps.Keys(p.Headers)) {
			hs = append(hs, [2]string{tomlQuote(k), tomlQuote(p.Headers[k])})
		}
		fields = append(fields, [2]string{"http_headers", tomlInline(hs)})
	}
	argv := []string{"-c", "model_provider=" + tomlQuote(id), "-c", "model=" + tomlQuote(model),
		"-c", "model_providers." + id + "=" + tomlInline(fields)}
	catalog := codexCatalogArgs(p)
	argv = append(argv, catalog...) // /model offers only ids this row serves
	if len(catalog) == 2 {
		env = append(env, settings.CodexCatalogEnv+"="+tomlString(strings.TrimPrefix(catalog[1], "model_catalog_json=")))
	}
	return env, codexHeadless(argv, h), nil
}

// login runs the agent on its own subscription sign-in; a login provider serves only the CLI it names.
func login(e engine, p *providers.Provider, model string, h *Headless) ([]string, []string, error) {
	if !e.loginServes(p) {
		return nil, nil, fmt.Errorf("%s: provider %s is the %s sign-in; use claude-code run %s --provider %s", e.Name(), p.Name, p.Auth.Login, p.Auth.Login, p.Name)
	}
	return e.login(p, model, h)
}

// codexLogin runs codex on its own ChatGPT sign-in: the built-in openai provider, no key injected.
func codexLogin(model string, h *Headless) ([]string, []string, error) {
	if err := codexChatGPTLogin(); err != nil {
		return nil, nil, err
	}
	argv := []string{"-c", "model_provider=" + tomlQuote("openai"), "-c", "model=" + tomlQuote(model)}
	return nil, codexHeadless(argv, h), nil
}

// codexHome is $CODEX_HOME or ~/.codex, where codex keeps auth.json and config.toml.
func codexHome() string {
	home, err := CodexHome()
	if err != nil {
		return filepath.Join(".codex") // no resolvable home: a relative path simply misses
	}
	return home
}

// codexOwnModel is the top-level `model = "..."` in codex's config.toml, or "" (a flat scan; sections are skipped).
func codexOwnModel() string {
	data, err := os.ReadFile(filepath.Join(codexHome(), "config.toml"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			return "" // top-level keys end at the first table header
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.Trim(strings.TrimSpace(k), `"'`) == "model" {
			return tomlString(v)
		}
	}
	return ""
}

// codexChatGPTLogin checks the installed CLI's login status, falling back to $CODEX_HOME/auth.json if unavailable.
func codexChatGPTLogin() error {
	if bin, err := paths.VendorBin("codex"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, "login", "status").CombinedOutput()
		if ctx.Err() == nil {
			text := strings.TrimSpace(string(out))
			switch {
			case err == nil && strings.Contains(strings.ToLower(text), "chatgpt"):
				return nil
			case err == nil:
				return fmt.Errorf("codex: signed in with an API key, not ChatGPT (%s); run `codex login` and choose \"Sign in with ChatGPT\"", text)
			default:
				return fmt.Errorf("codex: not signed in (%s); run `codex login` and choose \"Sign in with ChatGPT\"", text)
			}
		}
	}
	b, keyringNote := codexAuthJSON()
	var auth struct {
		Tokens json.RawMessage `json:"tokens"`
	}
	if b != nil && json.Unmarshal(b, &auth) == nil && len(auth.Tokens) > 0 && string(auth.Tokens) != "null" {
		return nil
	}
	file := filepath.Join(codexHome(), "auth.json")
	msg := fmt.Sprintf("codex: no ChatGPT sign-in in %s; run `codex login` and choose \"Sign in with ChatGPT\"", file)
	if keyringNote != "" {
		msg += fmt.Sprintf(" (keyring: %s)", keyringNote)
	}
	return errors.New(msg)
}

// codexHeadless appends the `codex exec` form when h is set; nil h is the interactive launch.
func codexHeadless(argv []string, h *Headless) []string {
	var effort string
	if h != nil {
		effort = h.Effort
	}
	if effort = cmp.Or(effort, providers.Pin(providers.PinCodexEffort)); effort != "" {
		argv = append(argv, "-c", "model_reasoning_effort="+tomlQuote(effort))
	}
	if h == nil {
		return argv
	}
	sandbox := "read-only"
	if h.Tools {
		sandbox = "workspace-write"
	}
	argv = append(append([]string{"exec"}, argv...), "--skip-git-repo-check", "--sandbox", sandbox)
	if h.NoPersist {
		argv = append(argv, "--ephemeral")
	}
	if h.SafeMode {
		argv = append(argv, "--ignore-user-config", "--ignore-rules")
	}
	if h.SessionID != "" {
		argv = append(argv, "resume", h.SessionID)
	}
	// The prompt rides stdin behind the `-` operand: a positional prompt like "help" is a codex subcommand.
	return append(argv, "-")
}

// CodexChatGPTLoginPresent reports whether codex's own ChatGPT sign-in (~/.codex/auth.json) is on disk.
func CodexChatGPTLoginPresent() bool { return codexChatGPTLogin() == nil }
