package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// --- codex: ~/.codex/config.toml, a banner-marked [model_providers.claude-code] block ---

const (
	codexManagedBegin = "# >>> claude-code:managed:begin >>>"
	codexManagedEnd   = "# <<< claude-code:managed:end <<<"
)

// codexBannerRE is compiled lazily: a package-level MustCompile runs at every process start (hooks, statusline).
var codexBannerRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?s)` + regexp.QuoteMeta(codexManagedBegin) + `\n.*?\n` + regexp.QuoteMeta(codexManagedEnd) + `\n?`)
})

func codexBannerSection(block string) string {
	return codexManagedBegin + "\n" + block + "\n" + codexManagedEnd + "\n"
}

// errNoBlock marks a deliberate "no banner" outcome, so the prior banner is stripped rather than kept.
var errNoBlock = errors.New("codex: no config.toml block")

func codexProviderSection(ctx context.Context, reg *providers.Registry, sel providers.Selection, existing string, out io.Writer) (string, error) {
	ctx = context.WithValue(ctx, codexFileAuthKey{}, true)
	sel, routed, err := codexRouteSelection(ctx, reg, sel, out)
	if err != nil {
		return "", err
	}
	target, note, err := codexSchema().ResolveTarget(ctx, agent.RunOptions{}, reg, sel, routed)
	if err != nil {
		fmt.Fprintf(out, "codex: skipped: %v\n", err)
		return "", nil
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if target.Provider != nil && target.Provider.Auth.Method() == providers.AuthCLI {
		return codexLoginSection(target.Provider.Name, existing, out)
	}
	target = codexRenderModel(ctx, reg, target, out)
	_, argv, err := codexSchema().Configure(ctx, target)
	if err != nil {
		fmt.Fprintf(out, "codex: skipped: %v\n", err)
		return "", nil
	}
	block, err := codexBlockFromArgv(argv)
	if err != nil {
		return "", fmt.Errorf("codex: %w", err)
	}
	keys, err := codexPermissionKeys(existing, out)
	if err != nil {
		return "", err
	}
	if keys != "" {
		block = keys + "\n" + block
	}
	return codexBannerSection(block), nil
}

// config_route values besides "": session follows the session profile, databricks the record's pool pick.
const (
	routeSession    = "session"
	routeDatabricks = providers.GroupDatabricks
)

// codexRouteProfiles are the Databricks profiles in reg that serve codex.
func codexRouteProfiles(reg *providers.Registry) []string {
	var out []string
	for _, n := range reg.ProfilesOf(providers.GroupDatabricks) {
		if agent.CanServe(agentid.Codex, reg.Providers[n]) {
			out = append(out, n)
		}
	}
	return out
}

// validateConfigRoute refuses a config_route that is not empty, session, databricks or a codex-serving Databricks profile.
func validateConfigRoute(reg *providers.Registry, v string) error {
	names := codexRouteProfiles(reg)
	if v == "" || v == routeSession || (v == routeDatabricks && len(names) > 0) || slices.Contains(names, v) {
		return nil
	}
	valid := append([]string{routeSession, routeDatabricks}, names...)
	return fmt.Errorf("codex: config_route %q: want one of %s", v, strings.Join(valid, ", "))
}

// ValidateConfigRoute is validateConfigRoute against the loaded registry.
func ValidateConfigRoute(v string) error {
	reg, err := providers.LoadShared()
	if err != nil {
		return err
	}
	return validateConfigRoute(reg, strings.TrimSpace(v))
}

// codexRouteSelection is sel, or the Selection of the Databricks profile the record's config_route names; routed is true then.
func codexRouteSelection(ctx context.Context, reg *providers.Registry, sel providers.Selection, out io.Writer) (providers.Selection, bool, error) {
	rec, err := agentconfig.Settings(agentid.Codex)
	if err != nil {
		return sel, false, fmt.Errorf("codex: agent settings: %w", err)
	}
	route := strings.TrimSpace(rec.Extras[extraConfigRoute])
	if route == "" || route == routeSession {
		return sel, false, nil
	}
	if err := validateConfigRoute(reg, route); err != nil {
		return sel, false, err
	}
	name := route
	if route == routeDatabricks {
		q := agent.QueryFor(agent.PeekContext(ctx), agent.ModelQuery{Agent: agentid.Codex, Profile: agent.AutoModel})
		r, err := agent.ResolveModel(reg, q, time.Now())
		if err != nil {
			return sel, false, fmt.Errorf("codex: config_route %s: %w", route, err)
		}
		name = r.Profile
	}
	routed, err := providers.Select(reg, name)
	if err != nil {
		return sel, false, fmt.Errorf("codex: config_route %s: %w", route, err)
	}
	fmt.Fprintf(out, "codex: config_route %s: writing the Databricks block for profile %s\n", route, name)
	return routed, true, nil
}

// codexRenderModel gives a Databricks target the model a launch runs: the pin, else a Responses model; peek-only.
func codexRenderModel(ctx context.Context, reg *providers.Registry, t providers.Target, out io.Writer) providers.Target {
	if t.Provider == nil || t.Provider.Preset != providers.PresetDatabricks {
		return t
	}
	pin := agent.AgentModelPin(agentid.Codex)
	if _, auto := agent.SplitAuto(pin); pin != "" && !auto {
		q := agent.QueryFor(agent.PeekContext(ctx), agent.ModelQuery{Agent: agentid.Codex, Profile: t.Provider.Name})
		if r, err := agent.ResolveRun(reg, q, time.Now()); err == nil && codexResponsesServable(t.Provider, r.Model) {
			fmt.Fprintf(out, "codex: wrote model %s on profile %s (the codex model pin)\n", r.Model, t.Provider.Name)
			t.Model = r.Model
			return t
		}
	}
	if m := codexResponsesModel(t.Provider, t.Model); m != t.Model {
		fmt.Fprintf(out, "codex: wrote model %s on profile %s (%s is not served over Responses; no usable pin)\n", m, t.Provider.Name, t.Model)
		t.Model = m
	}
	return t
}

func codexLoginSection(name, existing string, out io.Writer) (string, error) {
	keys, err := codexPermissionKeys(existing, out)
	if err != nil {
		return "", err
	}
	if keys == "" {
		fmt.Fprintf(out, "codex: skipped: %s runs on codex's own login; no config.toml block\n", name)
		return "", errNoBlock
	}
	fmt.Fprintf(out, "codex: %s runs on codex's own login; writing approval/sandbox keys only\n", name)
	return codexBannerSection(strings.TrimRight(keys, "\n")), nil
}

// codexCredentialStoreRE matches bare or quoted cli_auth_credentials_store assignments.
var codexCredentialStoreRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^\s*(?:"cli_auth_credentials_store"|'cli_auth_credentials_store'|cli_auth_credentials_store)\s*=`)
})

// codexUserSetsCredStore reports whether a top-level key of existing already assigns cli_auth_credentials_store.
func codexUserSetsCredStore(existing string) bool {
	lines := strings.SplitAfter(existing, "\n")
	if idx := agentconfig.FirstTopLevelHeaderIndex(lines); idx >= 0 {
		lines = lines[:idx] // a key under a table does not set the top-level one
	}
	multiline, depth := "", 0
	for _, line := range lines {
		if multiline == "" && depth == 0 && codexCredentialStoreRE().MatchString(line) {
			return true
		}
		multiline, depth = agentconfig.TOMLLineState(line, multiline, depth) // skip lines inside """/''' strings
	}
	return false
}

// codexCredStoreKey defaults to the OS keyring without overwriting an explicit user setting.
func codexCredStoreKey(existing string, out io.Writer) string {
	if codexUserSetsCredStore(existing) {
		fmt.Fprintln(out, "codex: cli_auth_credentials_store already set by the user; leaving it")
		return ""
	}
	return `cli_auth_credentials_store = "keyring"` + "\n"
}

// codexPermissionKeys renders explicit permissions and the credential store, failing on unreadable settings.
func codexPermissionKeys(existing string, out io.Writer) (string, error) {
	rec, err := agentconfig.Settings(agentid.Codex)
	if err != nil {
		return "", fmt.Errorf("codex: agent settings: %w", err)
	}
	var b strings.Builder
	if v := rec.Extras[extraApprovalPolicy]; v != "" {
		fmt.Fprintf(&b, "approval_policy = %q\n", v)
	}
	if v := rec.Extras[extraSandboxMode]; v != "" {
		fmt.Fprintf(&b, "sandbox_mode = %q\n", v)
	}
	b.WriteString(codexCredStoreKey(existing, out))
	return b.String(), nil
}

// codexBlockFromArgv rebuilds Configure overrides as TOML with the fixed provider id "claude-code".
func codexBlockFromArgv(argv []string) (string, error) {
	var model, tableVal, catalog string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] != "-c" {
			continue
		}
		k, v, ok := strings.Cut(argv[i+1], "=")
		if !ok {
			continue
		}
		switch {
		case k == "model":
			model = v
		case k == "model_catalog_json":
			catalog = v
		case strings.HasPrefix(k, "model_providers."):
			tableVal = v
		}
	}
	if tableVal == "" {
		return "", errors.New("no model_providers table in codex argv")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "model_provider = %q\n", "claude-code")
	fmt.Fprintf(&b, "model = %s\n", model)
	if catalog != "" {
		fmt.Fprintf(&b, "model_catalog_json = %s\n", catalog)
	}
	b.WriteString("\n")
	b.WriteString("[model_providers.claude-code]\n")
	for _, f := range agentconfig.SplitInlineTable(tableVal) {
		b.WriteString(f)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func renderCodex(ctx context.Context, reg *providers.Registry, sel providers.Selection, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	body, err := CodexJSON()
	if err != nil {
		return false, err
	}
	path, raw, existed, err := readCodexConfig(o.Home)
	if err != nil {
		return false, err
	}
	next, err := codexProviderConfig(ctx, reg, sel, string(raw), out)
	if err != nil {
		return false, err
	}

	// Compute the hooks merge (and read the existing hooks.json) before computing trust entries or committing config.toml
	hooksPath := filepath.Join(filepath.Dir(path), "hooks.json")
	hooksRaw, mergedHooks, hooksExisted, err := prepareCodexHooks(hooksPath, body)
	if err != nil {
		return false, err
	}

	next, err = codexTrustAndSettings(next, hooksPath, mergedHooks)
	if err != nil {
		return false, err
	}

	drift, err := commitCodexFiles(hooksPath, hooksRaw, mergedHooks, hooksExisted, path, raw, next, existed, o, out)
	if err == nil && !o.DryRun && !o.Check && codexRouteSig(string(raw)) != codexRouteSig(next) {
		fmt.Fprintf(out, "codex: provider, model or catalog changed in %s; restart a running ChatGPT app or Codex app-server\n", path)
	}
	return drift, err
}

// codexRouteSig is the top-level provider, model and catalog keys plus the provider table of the managed block.
func codexRouteSig(text string) string {
	keys := []string{"model_provider", "model", "model_catalog_json"}
	vals, _ := readCodexStrings(text, func(k string) bool { return slices.Contains(keys, k) })
	banner := codexBannerRE().FindString(text)
	_, table, _ := strings.Cut(banner, "[model_providers.")
	return fmt.Sprint(vals) + "\x00" + table
}

// commitCodexFiles writes hooks.json first, so a failed hooks write leaves config.toml without trust hashes for it.
func commitCodexFiles(hooksPath string, hooksRaw, mergedHooks []byte, hooksExisted bool,
	path string, raw []byte, next string, existed bool, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	drift, err := agentconfig.CommitFile(hooksPath, hooksRaw, agentconfig.HooksFileBytes(hooksRaw, mergedHooks), hooksExisted, o, out, "codex-hooks")
	if err != nil {
		return drift, err
	}
	d, err := agentconfig.CommitFile(path, raw, []byte(next), existed, o, out, "codex")
	return drift || d, err
}

func readCodexConfig(home string) (path string, raw []byte, existed bool, err error) {
	path, err = paths.CodexConfigPath(home)
	if err != nil {
		return "", nil, false, err
	}
	raw, existed, err = agentconfig.ReadExisting(path)
	if err != nil {
		return "", nil, false, fmt.Errorf("codex: %w", err)
	}
	return path, raw, existed, nil
}

// codexProviderConfig is raw with the provider section inserted, any prior banner removed.
func codexProviderConfig(ctx context.Context, reg *providers.Registry, sel providers.Selection, raw string, out io.Writer) (string, error) {
	stripped := codexBannerRE().ReplaceAllString(raw, "") // sans any prior banner, for the user-value check
	section, err := codexProviderSection(ctx, reg, sel, stripped, out)
	if errors.Is(err, errNoBlock) {
		return stripped, nil
	}
	if err != nil {
		return "", err
	}
	return insertCodexSection(raw, section), nil
}

func insertCodexSection(raw, section string) string {
	if section == "" {
		return raw
	}
	return agentconfig.InsertTopLevelTOML(codexBannerRE().ReplaceAllString(raw, ""), section)
}

func prepareCodexHooks(path string, body []byte) ([]byte, []byte, bool, error) {
	raw, existed, err := agentconfig.ReadExisting(path)
	if err != nil {
		return nil, nil, false, fmt.Errorf("codex-hooks: %w", err)
	}
	merged, err := mergeCodexHooks(raw, body)
	if err != nil {
		return nil, nil, false, fmt.Errorf("codex-hooks: %w", err)
	}
	return raw, merged, existed, nil
}

func codexTrustAndSettings(next, hooksPath string, mergedHooks []byte) (string, error) {
	entries, err := codexTrustEntries(hooksPath, mergedHooks)
	if err != nil {
		return "", err
	}
	next = mergeCodexTrust(next, entries)
	prefs, err := LoadCodexPreferences()
	if err != nil {
		return "", err
	}
	return patchCodexSettings(next, prefs)
}
