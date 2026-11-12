// overlay_rules.go — the refusal rules every overlay env entry (set, keep, unset, --env) passes.
package agentconfig

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// credentialName reports a credential-shaped name: AUTH anywhere, or a redaction segment (KEY, TOKEN, PAT, ...).
func credentialName(up string) bool {
	return strings.Contains(up, "AUTH") || providers.IsSecretName(up)
}

// rule is one refusal: the first whose match holds decides the result, even when its err returns nil.
type rule struct {
	match func(c *ruleCtx) bool
	err   func(c *ruleCtx) error
}

// ruleCtx is one name (and for set, its value) under test; up is the upper-cased name.
type ruleCtx struct {
	name, up, val, ref string
	isRef, reserved    bool
	keep               map[string]bool
	route              func(string) bool
}

func refuse(err error) func(*ruleCtx) error { return func(*ruleCtx) error { return err } }

func runRules(rules []rule, c *ruleCtx) error {
	for _, r := range rules {
		if r.match(c) {
			return r.err(c)
		}
	}
	return nil
}

// nameRules are the checks keep and unset names share: a malformed, route-redirecting or config-relocating name.
var nameRules = []rule{
	{func(c *ruleCtx) bool { return !vault.EnvName.MatchString(c.name) }, refuse(errEnvName)},
	{func(c *ruleCtx) bool { return routeRedirectName(c.up) }, refuse(errBaseURL)},
	{func(c *ruleCtx) bool { return configLocationName(c.up) }, refuse(errConfigLocation)},
}

var keepRules = slices.Concat(nameRules, []rule{
	{func(c *ruleCtx) bool { return strings.HasPrefix(c.up, "HARNESS_") || credentialName(c.up) },
		refuse(errors.New("a credential name cannot be kept: the scrub withholds it from every agent child"))}})

// unsetRules refuse dropping a name the launch itself sets for its route: a lost endpoint would keep the credential.
var unsetRules = slices.Concat(nameRules, []rule{
	{func(c *ruleCtx) bool { return strings.HasPrefix(c.up, "HARNESS_") },
		refuse(errors.New("harness-owned: the launch sets it for its route; use --profile"))}})

func nameRule(rules []rule, name string) error {
	return runRules(rules, &ruleCtx{name: name, up: strings.ToUpper(name)})
}

// flagSetRules are the --env rule set: a NAME=VALUE check, the shared name rules, then the value's credential shape.
var flagSetRules = slices.Concat([]rule{
	{func(c *ruleCtx) bool { return c.name == "" || strings.ContainsAny(c.name, " \t") },
		refuse(errors.New("want NAME=VALUE"))}},
	nameRules, // before the scrub check
	[]rule{
		{func(c *ruleCtx) bool { return envReserved(c.name, c.up) },
			refuse(errors.New("reserved by the harness credential scrub: use `claude-code vault set` or a provider's auth.command"))},
		{func(c *ruleCtx) bool { return strings.HasPrefix(c.up, OverlayVar) }, // a flag must not point at another overlay
			refuse(errors.New("harness-owned: pass an overlay with --overlay, not --env"))},
		{func(c *ruleCtx) bool { return credentialName(c.up) || textutil.LooksSecret(c.val) },
			refuse(errors.New("looks like a credential: store it with `claude-code vault set` or a provider's auth.command, not on the command line"))},
	})

var errBaseURL = errors.New("a route endpoint, proxy or TLS setting: pick the endpoint with --profile; " +
	"an override would send the route's credential elsewhere")

var errConfigLocation = errors.New("an agent config, permission or routing setting: the harness sets it per launch; use --profile")

// configLocationName reports a name that injects or relocates an agent's config or route; refused everywhere.
func configLocationName(up string) bool {
	switch up {
	case "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_TUI_CONFIG", proc.EnvCopilotProvidersConfig, "COPILOT_HOME",
		"COPILOT_ALLOW_ALL", "COPILOT_ASSISTED_APPROVAL", "COPILOT_CUSTOM_INSTRUCTIONS_DIRS",
		"COPILOT_API_URL", "COPILOT_GH_HOST", "CODEX_HOME", proc.EnvClaudeConfigDir, proc.EnvAnthropicCustomHeaders,
		"XDG_CONFIG_HOME", "GEMINI_CLI_HOME":
		return true
	}
	return up == "OPENCODE_PERMISSION" || strings.HasPrefix(up, "OPENCODE_DISABLE_") ||
		strings.HasSuffix(up, "_CONFIG_CONTENT") || strings.HasPrefix(up, proc.CopilotProviderPrefix)
}

// routeRedirectName reports a name that can redirect a route credential or weaken TLS; refused in set, keep and unset.
func routeRedirectName(up string) bool {
	switch up {
	case "DATABRICKS_HOST", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NODE_TLS_REJECT_UNAUTHORIZED",
		"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR", "AGY_LLM_GATEWAY_HEADERS":
		return true
	}
	return slices.ContainsFunc(redirectSuffixes, func(s string) bool { return strings.HasSuffix(up, s) })
}

var redirectSuffixes = []string{"_BASE_URL", "_API_BASE", "_ENDPOINT", "_GATEWAY_URL", "_PROXY_URL", "_CA_CERT"}

// envReserved reports a name (as typed or upper-cased) the harness credential scrub withholds.
func envReserved(name, up string) bool { return proc.EnvBlocked(name) || proc.EnvBlocked(up) }

var setRules = []rule{
	{func(c *ruleCtx) bool { return !vault.EnvName.MatchString(c.name) }, refuse(errEnvName)},
	{func(c *ruleCtx) bool {
		return strings.HasPrefix(c.up, "HARNESS_") || strings.HasPrefix(c.up, OverlayVar)
	},
		refuse(errors.New("harness-owned: the harness sets it per launch"))},
	{func(c *ruleCtx) bool { return routeRedirectName(c.up) }, refuse(errBaseURL)},
	{func(c *ruleCtx) bool { return configLocationName(c.up) }, refuse(errConfigLocation)},
	{func(c *ruleCtx) bool { return c.reserved && c.isRef },
		refuse(errors.New("reserved by the credential scrub: a vault ref cannot replace a route value"))},
	{func(c *ruleCtx) bool { return c.reserved && nameRule(keepRules, c.name) != nil }, // keep refuses it too
		refuse(errors.New("reserved by the credential scrub: no overlay can pass it (keep and @secret refs are refused too)"))},
	{func(c *ruleCtx) bool { return c.reserved && !c.keep[c.up] },
		refuse(errors.New("reserved by the credential scrub: add it to keep to pass your own value"))},
	{func(c *ruleCtx) bool { return c.isRef }, func(c *ruleCtx) error { return refRule(c.ref, c.route) }},
	{func(c *ruleCtx) bool { return strings.ContainsRune(c.val, 0) }, refuse(errors.New("value holds a NUL byte"))},
	{func(c *ruleCtx) bool { return strings.HasPrefix(c.val, providers.SecretRef) },
		refuse(errors.New("malformed @secret reference: want @secret or @secret:<NAME>"))},
	{func(c *ruleCtx) bool { return credentialName(c.up) || textutil.LooksSecret(c.val) },
		refuse(errors.New("looks like a credential: store it with `claude-code vault set`, reference it as @secret:<NAME>"))},
}

// setRule checks one set entry against rules; route reports a profile's route credential (providers.RouteSecretGuard).
func setRule(rules []rule, name, val string, keep map[string]bool, route func(string) bool) error {
	up := strings.ToUpper(name)
	ref, isRef := providers.SecretRefName(name, val)
	c := &ruleCtx{name: name, up: up, val: val, ref: ref, isRef: isRef, keep: keep, route: route,
		reserved: envReserved(name, up)}
	return runRules(rules, c)
}

// refRule refuses a vault name an overlay may not pass on: harness-internal secrets and any profile's route credential.
func refRule(ref string, route func(string) bool) error {
	switch {
	case !vault.EnvName.MatchString(ref):
		return errors.New("want @secret:<NAME> with an env-style NAME")
	case providers.InternalSecretName(ref):
		return fmt.Errorf("@secret:%s is harness-internal", ref)
	case route(ref):
		return fmt.Errorf("@secret:%s is a profile's route credential: use --profile", ref)
	}
	return nil
}
