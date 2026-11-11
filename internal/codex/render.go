package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

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

func codexProviderSection(reg *providers.Registry, sel providers.Selection, existing string, out io.Writer) (string, error) {
	ctx := context.Background()
	target, note, err := codexSchema().ResolveTarget(reg, sel, false)
	if err != nil {
		fmt.Fprintf(out, "codex: skipped: %v\n", err)
		return "", nil
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if target.Provider != nil && target.Provider.Auth.Method() == providers.AuthCLI {
		keys, err := codexPermissionKeys(existing, out)
		if err != nil {
			return "", err
		}
		if keys == "" {
			fmt.Fprintf(out, "codex: skipped: %s runs on codex's own login; no config.toml block\n", target.Provider.Name)
			return "", nil
		}
		fmt.Fprintf(out, "codex: %s runs on codex's own login; writing approval/sandbox keys only\n", target.Provider.Name)
		return codexBannerSection(strings.TrimRight(keys, "\n")), nil
	}
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

// codexCredentialStoreRE matches one line assigning cli_auth_credentials_store, bare, single- or double-quoted, indented or not.
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

// codexCredStoreKey keeps ~/.codex/auth.json out of plaintext by pointing codex at the OS keyring;
// it never overwrites a value the user already set outside the managed banner.
func codexCredStoreKey(existing string, out io.Writer) string {
	if codexUserSetsCredStore(existing) {
		fmt.Fprintln(out, "codex: cli_auth_credentials_store already set by the user; leaving it")
		return ""
	}
	return `cli_auth_credentials_store = "keyring"` + "\n"
}

// codexPermissionKeys renders approval_policy/sandbox_mode/cli_auth_credentials_store as top-level TOML keys;
// approval/sandbox skip when unset. A failed settings load is an error, so the render never drops them.
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

// codexBlockFromArgv rebuilds a `[model_providers.claude-code]` TOML block from the `-c key=value`
// pairs the Configure(codex) result returns, renaming the provider id to the fixed "claude-code".
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

func renderCodex(reg *providers.Registry, sel providers.Selection, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	body, err := CodexJSON()
	if err != nil {
		return false, err
	}
	path, raw, existed, err := readCodexConfig(o.Home)
	if err != nil {
		return false, err
	}
	next, err := codexProviderConfig(reg, sel, string(raw), out)
	if err != nil {
		return false, err
	}

	// Compute the hooks merge (and read the existing hooks.json) before computing trust entries or
	// committing config.toml: trust keys must come from the real post-merge positions, and a malformed
	// existing hooks.json must fail the whole render, not leave new trust hashes written against a
	// hooks.json that never got merged.
	hooksPath := filepath.Join(filepath.Dir(path), "hooks.json")
	hooksRaw, mergedHooks, hooksExisted, err := prepareCodexHooks(hooksPath, body)
	if err != nil {
		return false, err
	}

	next, err = codexTrustAndSettings(next, hooksPath, mergedHooks)
	if err != nil {
		return false, err
	}

	return commitCodexFiles(hooksPath, hooksRaw, mergedHooks, hooksExisted, path, raw, next, existed, o, out)
}

// commitCodexFiles writes hooks.json first, so a failed hooks write leaves config.toml without trust hashes for it.
func commitCodexFiles(hooksPath string, hooksRaw, mergedHooks []byte, hooksExisted bool,
	path string, raw []byte, next string, existed bool, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	drift, err := agentconfig.CommitFile(hooksPath, hooksRaw, append(mergedHooks, '\n'), hooksExisted, o, out, "codex-hooks")
	if err != nil {
		return drift, err
	}
	d, err := agentconfig.CommitFile(path, raw, []byte(next), existed, o, out, "codex")
	return drift || d, err
}

// readCodexConfig returns the config.toml path, its bytes and whether it existed.
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
func codexProviderConfig(reg *providers.Registry, sel providers.Selection, raw string, out io.Writer) (string, error) {
	stripped := codexBannerRE().ReplaceAllString(raw, "") // sans any prior banner, for the user-value check
	section, err := codexProviderSection(reg, sel, stripped, out)
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

// writeCodexHooks writes hooks.json under a file lock, refusing when it changed since the merge read it.
