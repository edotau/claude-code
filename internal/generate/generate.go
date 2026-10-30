// Package generate renders agent configs and hooks without launching a session.
package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent/vendors"
	"github.com/edotau/claude-code/internal/codexsettings"
	"github.com/edotau/claude-code/internal/hooks/hookspec"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// DefaultAgents is every vendor generate renders when Options.Agents is empty.
var DefaultAgents = []string{"codex", "opencode", "gemini", "copilot"}

// Options controls one generate pass.
type Options struct {
	Agents  []string  // vendors to render; default DefaultAgents
	DryRun  bool      // print the rendered blocks; write nothing
	Check   bool      // report drift; never writes
	Home    string    // overrides os.UserHomeDir() for vendor config paths (tests)
	Out     io.Writer // status lines; nil -> os.Stdout
	Vendors bool      // force vendor-config renders even when CLAUDE_CONFIG_DIR is not the default ~/.claude
	// Confirm asks whether to overwrite path given its lost hand-edited lines; nil means write and warn.
	Confirm func(path string, lost []string) bool
	// Prepare's write queue: commitFile and the JSONC retire append to it, Apply runs it.
	staged *[]func() error
}

// Prepared holds ordered vendor writes after all selected renders have validated.
type Prepared struct {
	writes []func() error
}

// Apply performs the staged writes in order, stopping at the first error.
func (p *Prepared) Apply() error {
	for _, write := range p.writes {
		if err := write(); err != nil {
			return err
		}
	}
	return nil
}

// Run renders every selected vendor's config and reports whether anything
// differs from what is on disk (Check) or would be written (DryRun never writes either way).
func Run(o Options) (bool, error) {
	p, drift, err := Prepare(o)
	if err != nil {
		return drift, err
	}
	return drift, p.Apply()
}

// Prepare validates all selected renders without writing; dry-run/check output remains immediate.
func Prepare(o Options) (*Prepared, bool, error) {
	p := &Prepared{}
	o.staged = &p.writes
	var drift bool
	var err error
	agents := o.Agents
	if len(agents) == 0 {
		agents = DefaultAgents
	}
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	home := o.Home
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, false, fmt.Errorf("generate: %w", err)
		}
	}
	if !o.Vendors && !configDirIsDefault() {
		fmt.Fprintf(out, "generate: skipping agent configs (CLAUDE_CONFIG_DIR=%s is not the default ~/.claude); pass --vendors to include them\n", paths.ConfigDir())
		return p, false, nil
	}
	reg, err := providers.Load()
	if err != nil {
		return nil, false, fmt.Errorf("generate: %w", err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		return nil, false, fmt.Errorf("generate: %w", err)
	}
	want := map[string]bool{}
	for _, a := range agents {
		want[a] = true
	}
	for _, name := range DefaultAgents {
		if !want[name] {
			continue
		}
		var d bool
		switch name {
		case "codex":
			d, err = renderCodex(reg, sel, o, out)
		case "opencode":
			d, err = renderOpenCodeOverlay(o, out)
		case "gemini":
			d, err = reportGemini(sel, out)
			if err == nil {
				var gd bool
				gd, err = renderGeminiSettings(home, o, out)
				d = d || gd
			}
		case "copilot":
			d, err = renderCopilot(reg, sel, home, o, out)
		}
		if err != nil {
			return nil, drift, err
		}
		drift = drift || d
	}
	return p, drift, nil
}

// configDirIsDefault reports whether CLAUDE_CONFIG_DIR resolves to the default ~/.claude for the
// process's real HOME: a worktree or test run that points CLAUDE_CONFIG_DIR elsewhere (while vendor
// configs stay $HOME-rooted) must not write into the real vendor homes without --vendors.
func configDirIsDefault() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	return paths.ConfigDir() == filepath.Join(home, ".claude")
}

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
	target, note, err := vendors.ResolveTarget(reg, "codex", sel, false)
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
	_, argv, err := vendors.Configure(ctx, "codex", target, "")
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
// TOML keys; approval/sandbox skip when unset, a vendorconfig load error is treated as unset.
func codexPermissionKeys(existing string, out io.Writer) string {
	cfg, err := vendors.Load()
	if err != nil {
		fmt.Fprintf(out, "codex: vendorconfig: %v\n", err)
		return ""
	}
	perm := cfg.Vendor("codex").Permission
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
// pairs vendors.Configure("codex", ...) returns, renaming the provider id to the fixed "claude-code".
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

// splitTopLevelInline splits a TOML inline table "{ k = v, k2 = { ... } }" on its top-level commas.
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
	for i, r := range s {
		switch r {
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

// commitFile is the shared dry-run/check/write decision once a renderer has the next byte slice.
func commitFile(path string, raw, next []byte, existed bool, o Options, out io.Writer, label string) (bool, error) {
	changed := !bytes.Equal(next, raw)
	switch {
	case o.DryRun:
		fmt.Fprintf(out, "--- %s (%s) ---\n%s\n", path, label, next)
		return changed, nil
	case o.Check:
		if changed {
			fmt.Fprintf(out, "%s: drift at %s\n", label, path)
		}
		lost, err := lostLinesFor(label, path, raw, next)
		if err != nil {
			return changed, err
		}
		if len(lost) > 0 {
			fmt.Fprintf(out, "%s: %s has %d hand-edited line(s) not in this render:\n", label, path, len(lost))
			printLostLines(out, lost)
		}
		return changed, nil
	case !changed:
		if label == "codex" {
			base, err := codexsettings.LoadBaseline(path)
			if err != nil {
				return false, err
			}
			if base == nil && existed {
				*o.staged = append(*o.staged, func() error { return codexsettings.SaveBaseline(path, next) })
			}
			fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
			return false, nil
		}
		// Seed a missing baseline from an unchanged file, or later hand edits go undetected.
		if _, err := os.Stat(vendorBaselinePath(label, path)); os.IsNotExist(err) && existed {
			_ = writeVendorBaseline(label, path, next)
		}
		fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
		return false, nil
	}
	*o.staged = append(*o.staged, func() error { return commitWrite(path, raw, next, existed, o, out, label) })
	return true, nil
}

// commitWrite runs the lost-lines check (prompting or warning), writes path, and refreshes its
// baseline; it is the body of every commitFile staged write.
func commitWrite(path string, raw, next []byte, existed bool, o Options, out io.Writer, label string) error {
	lost, err := lostLinesFor(label, path, raw, next)
	if err != nil {
		return err
	}
	if len(lost) > 0 {
		if o.Confirm != nil {
			fmt.Fprintf(out, "%s: %s has %d hand-edited line(s) that would be lost:\n", label, path, len(lost))
			printLostLines(out, lost)
			if !o.Confirm(path, lost) {
				fmt.Fprintf(out, "%s: kept %s (your edits)\n", label, path)
				return nil
			}
		} else {
			fmt.Fprintf(out, "%s: overwrote %d hand-edited line(s) in %s; `claude-code configure --revert` restores\n",
				label, len(lost), path)
			printLostLines(out, lost)
		}
	}
	write := backupAndWrite
	if label == "codex" {
		write = writeCodexConfig
	}
	if err := write(path, raw, next, existed); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := writeVendorBaseline(label, path, next); err != nil {
		fmt.Fprintf(out, "%s: baseline: %v (next run can't detect hand edits)\n", label, err)
	}
	fmt.Fprintf(out, "%s: wrote %s\n", label, path)
	return nil
}

// --- opencode: a tracked overlay (state/harness/opencode/{opencode,tui}.json), never opencode's own
// ~/.config/opencode files; the harness only writes those for the custom theme (see internal/ui/theme). ---

// renderOpenCodeOverlay writes the tracked opencode.json/tui.json overlay templates.
func renderOpenCodeOverlay(o Options, out io.Writer) (bool, error) {
	cfgPath, cfgBody, tuiPath, tuiBody, err := vendors.OpencodeOverlayRendered()
	if err != nil {
		return false, fmt.Errorf("opencode: %w", err)
	}
	cfgRaw, _ := os.ReadFile(cfgPath)
	tuiRaw, _ := os.ReadFile(tuiPath)
	cfgChanged, err := commitOverlayFile(cfgPath, cfgRaw, cfgBody, o, out, "opencode")
	if err != nil {
		return false, err
	}
	tuiChanged, err := commitOverlayFile(tuiPath, tuiRaw, tuiBody, o, out, "opencode-tui")
	if err != nil {
		return false, err
	}
	return cfgChanged || tuiChanged, nil
}

// commitOverlayFile is commitFile's dry-run/check/write decision, minus the lost-hand-edited-lines
// check and baseline tracking: the overlay is never hand-edited, so there is nothing to lose.
func commitOverlayFile(path string, raw, next []byte, o Options, out io.Writer, label string) (bool, error) {
	changed := !bytes.Equal(raw, next)
	switch {
	case o.DryRun:
		fmt.Fprintf(out, "--- %s (%s) ---\n%s\n", path, label, next)
		return changed, nil
	case o.Check:
		if changed {
			fmt.Fprintf(out, "%s: drift at %s\n", label, path)
		}
		return changed, nil
	case !changed:
		fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
		return false, nil
	}
	*o.staged = append(*o.staged, func() error {
		if _, err := paths.WriteIfChanged(path, next, 0o600); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		fmt.Fprintf(out, "%s: wrote %s\n", label, path)
		return nil
	})
	return true, nil
}

// --- gemini: report-only. agy (the vendor's binary, name unchanged) has no known settings-file
// auth-type knob like the old gemini-cli's security.auth.selectedType, so there is nothing to render;
// auth is the cached Google login marker at launch time (see vendors.GeminiLoginPresent).

func reportGemini(sel providers.Selection, out io.Writer) (bool, error) {
	if sel.Provider.Auth.Method() == providers.AuthCLI {
		fmt.Fprintln(out, "gemini: skipped: session provider is passthrough, not keyed")
		return false, nil
	}
	fmt.Fprintln(out, "gemini: no config render; agy has no settings-auth equivalent (launch-time env/login only)")
	return false, nil
}

// renderGeminiSettings merges the spec's exact-match guard denies into agy's
// ~/.gemini/antigravity-cli/settings.json permissions.deny, preserving every other key.
func renderGeminiSettings(home string, o Options, out io.Writer) (bool, error) {
	cfg, err := vendors.Load()
	if err != nil {
		fmt.Fprintf(out, "gemini-settings: skipped: %v\n", err)
		return false, nil
	}
	if !cfg.Vendor("gemini").Permission.UseGuardRules {
		return false, nil
	}
	var rules []string
	for _, r := range cfg.Guard().Deny {
		cmd, ok := strings.CutPrefix(r, "Bash(")
		cmd, ok2 := strings.CutSuffix(cmd, ")")
		if !ok || !ok2 || strings.HasSuffix(cmd, ":*") {
			fmt.Fprintf(out, "gemini-settings: note: %q is not exact-match; agy denies exact commands only, skipped\n", r)
			continue
		}
		rules = append(rules, "command("+cmd+")")
	}
	path := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	raw, readErr := os.ReadFile(path)
	existed := readErr == nil
	next := map[string]any{}
	if existed {
		if err := json.Unmarshal(raw, &next); err != nil {
			fmt.Fprintf(out, "gemini-settings: skipped: %s is not plain JSON: %v\n", path, err)
			return false, nil
		}
		if next == nil { // "null" decodes without error but leaves next nil, not an empty object
			fmt.Fprintf(out, "gemini-settings: skipped: %s is not plain JSON: not an object\n", path)
			return false, nil
		}
	}
	perms, _ := next["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	existing, _ := perms["deny"].([]any)
	seen := map[string]bool{}
	merged := make([]string, 0, len(existing)+len(rules))
	for _, e := range existing {
		s, ok := e.(string)
		if !ok || seen[s] {
			continue
		}
		seen[s] = true
		merged = append(merged, s)
	}
	for _, r := range rules {
		if seen[r] {
			continue
		}
		seen[r] = true
		merged = append(merged, r)
	}
	perms["deny"] = merged
	next["permissions"] = perms
	body, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return false, fmt.Errorf("gemini-settings: %w", err)
	}
	return commitFile(path, raw, append(body, '\n'), existed, o, out, "gemini-settings")
}

// Copilot hooks load from the user directory; plugin preToolUse hooks do not fire.

func renderCopilot(reg *providers.Registry, sel providers.Selection, home string, o Options, out io.Writer) (bool, error) {
	body, err := hookspec.CopilotJSON()
	if err != nil {
		return false, err
	}
	path := filepath.Join(home, ".copilot", "hooks", "claude-code-harness.json")
	drift, err := renderHooksFile(path, body, mergeCopilotHooks, o, out, "copilot-hooks")
	if err != nil {
		return drift, err
	}
	sd, err := renderCopilotSettings(home, o, out)
	if err != nil {
		return drift, err
	}
	drift = drift || sd
	ctx := context.Background()
	target, note, err := vendors.ResolveTarget(reg, "copilot", sel, false)
	if err != nil {
		fmt.Fprintf(out, "copilot: skipped: %v\n", err)
		return drift, nil
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if _, _, err := vendors.Configure(ctx, "copilot", target, ""); err != nil {
		fmt.Fprintf(out, "copilot: skipped: %v\n", err)
		return drift, nil
	}
	fmt.Fprintf(out, "copilot: launch-time env only; route = %s (%s)\n", target.Provider.Name, target.Model)
	return drift, nil
}

// renderCopilotSettings merges defaultPermissionMode from the vendor spec into ~/.copilot/settings.json,
// keeping every other key untouched; a non-JSON existing file is skipped, never clobbered.
func renderCopilotSettings(home string, o Options, out io.Writer) (bool, error) {
	cfg, err := vendors.Load()
	if err != nil {
		fmt.Fprintf(out, "copilot-settings: skipped: %v\n", err)
		return false, nil
	}
	mode := cfg.Vendor("copilot").Permission.DefaultPermissionMode
	if mode == "" {
		return false, nil
	}
	path := filepath.Join(home, ".copilot", "settings.json")
	raw, readErr := os.ReadFile(path)
	existed := readErr == nil
	next := map[string]any{}
	if existed {
		if err := json.Unmarshal(raw, &next); err != nil {
			fmt.Fprintf(out, "copilot-settings: skipped: %s is not plain JSON: %v\n", path, err)
			return false, nil
		}
		if next == nil { // "null" decodes without error but leaves next nil, not an empty object
			fmt.Fprintf(out, "copilot-settings: skipped: %s is not plain JSON: not an object\n", path)
			return false, nil
		}
	}
	next["defaultPermissionMode"] = mode
	body, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return false, fmt.Errorf("copilot-settings: %w", err)
	}
	return commitFile(path, raw, append(body, '\n'), existed, o, out, "copilot-settings")
}
