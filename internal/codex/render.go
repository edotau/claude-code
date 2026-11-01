package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
)

// --- codex: ~/.codex/config.toml, a banner-marked [model_providers.claude-code] block ---

// codexBannerRE is compiled lazily: a package-level MustCompile runs at every process start (hooks, statusline).
var codexBannerRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?s)# >>> claude-code:managed:begin >>>\n.*?\n# <<< claude-code:managed:end <<<\n?`)
})

func codexBannerSection(block string) string {
	return "# >>> claude-code:managed:begin >>>\n" + block + "\n# <<< claude-code:managed:end <<<\n"
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
		keys := codexPermissionKeys(existing, out)
		if keys == "" {
			fmt.Fprintf(out, "codex: skipped: %s runs on codex's own login; no config.toml block\n", target.Provider.Name)
			return "", nil
		}
		fmt.Fprintf(out, "codex: %s runs on codex's own login; writing approval/sandbox keys only\n", target.Provider.Name)
		return codexBannerSection(strings.TrimRight(keys, "\n")), nil
	}
	_, argv, err := codexSchema().Configure(ctx, target, "")
	if err != nil {
		fmt.Fprintf(out, "codex: skipped: %v\n", err)
		return "", nil
	}
	block, err := codexBlockFromArgv(argv)
	if err != nil {
		return "", fmt.Errorf("codex: %w", err)
	}
	if keys := codexPermissionKeys(existing, out); keys != "" {
		block = keys + "\n" + block
	}
	return codexBannerSection(block), nil
}

// codexCredentialStoreRE matches a top-level cli_auth_credentials_store key already set by the user.
var codexCredentialStoreRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^cli_auth_credentials_store\s*=`)
})

// codexCredStoreKey keeps ~/.codex/auth.json out of plaintext by pointing codex at the OS keyring;
// it never overwrites a value the user already set outside the managed banner.
func codexCredStoreKey(existing string, out io.Writer) string {
	if codexCredentialStoreRE().MatchString(existing) {
		fmt.Fprintln(out, "codex: cli_auth_credentials_store already set by the user; leaving it")
		return ""
	}
	return `cli_auth_credentials_store = "keyring"` + "\n"
}

// codexPermissionKeys renders approval_policy/sandbox_mode/cli_auth_credentials_store as top-level
// TOML keys; approval/sandbox skip when unset, an agent config load error is treated as unset.
func codexPermissionKeys(existing string, out io.Writer) string {
	cfg, err := agentconfig.LoadConfig()
	if err != nil {
		fmt.Fprintf(out, "codex: vendorconfig: %v\n", err)
		return ""
	}
	perm := cfg.Agent("codex").Permission
	var b strings.Builder
	if perm.ApprovalPolicy != "" {
		fmt.Fprintf(&b, "approval_policy = %q\n", perm.ApprovalPolicy)
	}
	if perm.SandboxMode != "" {
		fmt.Fprintf(&b, "sandbox_mode = %q\n", perm.SandboxMode)
	}
	b.WriteString(codexCredStoreKey(existing, out))
	return b.String()
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
	for _, f := range splitTopLevelInline(tableVal) {
		b.WriteString(f + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// splitTopLevelInline splits a TOML inline table "{ k = v, k2 = { ... } }" on its top-level commas, ignoring any inside quoted strings.
func splitTopLevelInline(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var parts []string
	depth, start := 0, 0
	var quote rune // the open string delimiter (" or '), 0 outside a string
	escaped := false
	for i, r := range s {
		if quote != 0 {
			switch {
			case escaped:
				escaped = false
			case r == '\\' && quote == '"':
				escaped = true
			case r == quote:
				quote = 0
			}
			continue
		}
		switch r {
		case '"', '\'':
			quote = r
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(parts, strings.TrimSpace(s[start:]))
}
