// Package generate renders every vendor agent's own config from the provider registry — codex
// ~/.codex/config.toml, opencode ~/.config/opencode/opencode.json, gemini (report-only: agy has no
// known settings-auth equivalent), copilot (report-only, launch-time env), in one pass,
// with no launch. internal/cli's `generate`/`settings` verb renders settings.json itself; this
// package is the vendor-config half called alongside it.
package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendors"
)

// DefaultAgents is every vendor generate renders when Options.Agents is empty.
var DefaultAgents = []string{"codex", "opencode", "gemini", "copilot"}

// Options controls one generate pass.
type Options struct {
	Agents []string  // vendors to render; default DefaultAgents
	DryRun bool      // print the rendered blocks; write nothing
	Check  bool      // report drift; never writes
	Home   string    // overrides os.UserHomeDir() for vendor config paths (tests)
	Out    io.Writer // status lines; nil -> os.Stdout
}

// Run renders every selected vendor's config and reports whether anything
// differs from what is on disk (Check) or would be written (DryRun never writes either way).
func Run(o Options) (drift bool, err error) {
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
			return false, fmt.Errorf("generate: %w", err)
		}
	}
	reg, err := providers.Load()
	if err != nil {
		return false, fmt.Errorf("generate: %w", err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		return false, fmt.Errorf("generate: %w", err)
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
			d, err = renderCodex(reg, sel, home, o, out)
		case "opencode":
			d, err = renderOpenCode(reg, sel, home, o, out)
			if err == nil {
				var td bool
				td, err = renderOpenCodeTUI(home, o, out)
				d = d || td
			}
		case "gemini":
			d, err = reportGemini(sel, out)
		case "copilot":
			d, err = reportCopilot(reg, sel, out)
		}
		if err != nil {
			return drift, err
		}
		drift = drift || d
	}
	return drift, nil
}

// backupAndWrite writes a single `.backup` of oldRaw (only when the file already existed) before
// atomically replacing path with newRaw.
func backupAndWrite(path string, oldRaw, newRaw []byte, existed bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if existed {
		if err := os.WriteFile(path+".backup", oldRaw, 0o600); err != nil {
			return err
		}
	}
	return paths.AtomicWrite(path, newRaw, 0o600)
}

// --- codex: ~/.codex/config.toml, a banner-marked [model_providers.claude-code] block ---

var codexBannerRE = regexp.MustCompile(`(?s)# >>> claude-code:managed:begin >>>\n.*?\n# <<< claude-code:managed:end <<<\n?`)

func codexBannerSection(block string) string {
	return "# >>> claude-code:managed:begin >>>\n" + block + "\n# <<< claude-code:managed:end <<<\n"
}

func renderCodex(reg *providers.Registry, sel providers.Selection, home string, o Options, out io.Writer) (bool, error) {
	ctx := context.Background()
	target, note, err := vendors.ResolveTarget(reg, "codex", sel, false)
	if err != nil {
		fmt.Fprintf(out, "codex: skipped: %v\n", err)
		return false, nil
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if target.Provider != nil && target.Provider.Auth.Type == providers.AuthPassthrough {
		fmt.Fprintf(out, "codex: skipped: %s runs on codex's own login; no config.toml block\n", target.Provider.Name)
		return false, nil
	}
	_, argv, err := vendors.Configure(ctx, "codex", target, "")
	if err != nil {
		fmt.Fprintf(out, "codex: skipped: %v\n", err)
		return false, nil
	}
	block, err := codexBlockFromArgv(argv)
	if err != nil {
		return false, fmt.Errorf("codex: %w", err)
	}
	path := filepath.Join(home, ".codex", "config.toml")
	return spliceBannerFile(path, codexBannerSection(block), codexBannerRE, o, out, "codex")
}

// codexBlockFromArgv rebuilds a `[model_providers.claude-code]` TOML block from the `-c key=value`
// pairs vendors.Configure("codex", ...) returns, renaming the provider id to the fixed "claude-code".
func codexBlockFromArgv(argv []string) (string, error) {
	var model, tableVal string
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
		case strings.HasPrefix(k, "model_providers."):
			tableVal = v
		}
	}
	if tableVal == "" {
		return "", errors.New("no model_providers table in codex argv")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "model_provider = %q\n", "claude-code")
	fmt.Fprintf(&b, "model = %s\n\n", model)
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

// spliceBannerFile replaces the text between banner markers (appending them when absent), leaving
// every other byte of path untouched.
func spliceBannerFile(path, section string, bannerRE *regexp.Regexp, o Options, out io.Writer, label string) (bool, error) {
	raw, readErr := os.ReadFile(path)
	existed := readErr == nil
	var next []byte
	switch {
	case existed && bannerRE.MatchString(string(raw)):
		next = []byte(bannerRE.ReplaceAllString(string(raw), section))
	case existed:
		sep := "\n"
		if len(raw) == 0 || raw[len(raw)-1] == '\n' {
			sep = ""
		}
		next = append(append(append([]byte{}, raw...), []byte(sep+"\n")...), []byte(section)...)
	default:
		next = []byte(section)
	}
	return commitFile(path, raw, next, existed, o, out, label)
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
		return changed, nil
	case !changed:
		fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
		return false, nil
	}
	if err := backupAndWrite(path, raw, next, existed); err != nil {
		return false, fmt.Errorf("%s: %w", label, err)
	}
	fmt.Fprintf(out, "%s: wrote %s\n", label, path)
	return true, nil
}

// --- opencode: ~/.config/opencode/opencode.json, reusing vendors.Configure's own render ---

// openCodeOwnedKeys are the top-level keys this tool sets; "x-claude-code-managed" names them in the
// file itself (JSON has no comment syntax, so this key IS the banner).
var openCodeOwnedKeys = []string{"$schema", "model", "permission", "mcp"}

// openCodeCopilotOwnedKeys is openCodeOwnedKeys for the github-copilot route: model/small_model instead
// of a provider block, since that login is opencode's own, not a harness-authored one.
var openCodeCopilotOwnedKeys = []string{"$schema", "model", "small_model", "permission", "mcp"}

// opencodeJSONPath is ~/.config/opencode/opencode.json, the file every opencode render below writes.
func opencodeJSONPath(home string) string {
	return filepath.Join(home, ".config", "opencode", "opencode.json")
}

// retireStaleOpenCodeJSONC moves a conflicting opencode.jsonc out of the way before writing
// opencode.json: opencode prefers the .jsonc form when both exist, so a config this tool writes to
// .json would otherwise be silently ignored. Never deletes; --dry-run/--check report without moving.
func retireStaleOpenCodeJSONC(home string, o Options, out io.Writer) error {
	jsoncPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	if _, err := os.Stat(jsoncPath); err != nil {
		return nil
	}
	bak := jsoncPath + ".bak-" + time.Now().UTC().Format("20060102T150405Z")
	if o.DryRun || o.Check {
		fmt.Fprintf(out, "opencode: would move %s -> %s (conflicts with opencode.json)\n", jsoncPath, bak)
		return nil
	}
	if err := os.Rename(jsoncPath, bak); err != nil {
		return fmt.Errorf("opencode: %w", err)
	}
	fmt.Fprintf(out, "opencode: moved %s -> %s (conflicts with opencode.json)\n", jsoncPath, bak)
	return nil
}

func renderOpenCode(reg *providers.Registry, sel providers.Selection, home string, o Options, out io.Writer) (bool, error) {
	ctx := context.Background()
	target, note, err := vendors.ResolveTarget(reg, "opencode", sel, false)
	if err != nil {
		fmt.Fprintf(out, "opencode: skipped: %v\n", err)
		return false, nil
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if err := retireStaleOpenCodeJSONC(home, o, out); err != nil {
		return false, err
	}
	if target.Provider != nil && target.Provider.Name == "github-copilot" {
		return renderOpenCodeCopilot(reg, home, o, out)
	}
	env, _, err := vendors.Configure(ctx, "opencode", target, "")
	if err != nil {
		fmt.Fprintf(out, "opencode: skipped: %v\n", err)
		return false, nil
	}
	var content string
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "OPENCODE_CONFIG_CONTENT" {
			content = v
		}
	}
	if content == "" {
		fmt.Fprintln(out, "opencode: skipped: no config content resolved")
		return false, nil
	}
	var rendered map[string]any
	if err := json.Unmarshal([]byte(content), &rendered); err != nil {
		return false, fmt.Errorf("opencode: %w", err)
	}
	path := opencodeJSONPath(home)
	raw, readErr := os.ReadFile(path)
	existed := readErr == nil
	var existing map[string]any
	if existed {
		_ = json.Unmarshal(raw, &existing) // a hand-broken file is treated as empty; the write below fixes it
	}
	next := map[string]any{}
	maps.Copy(next, existing)
	for _, k := range openCodeOwnedKeys {
		if v, ok := rendered[k]; ok {
			next[k] = v
		} else {
			delete(next, k)
		}
	}
	// provider is merged key-by-key so a foreign provider id the user configured by hand survives.
	merged := map[string]any{}
	if ep, ok := existing["provider"].(map[string]any); ok {
		maps.Copy(merged, ep)
	}
	if rp, ok := rendered["provider"].(map[string]any); ok {
		maps.Copy(merged, rp)
	}
	next["provider"] = merged
	managed := append(append([]string{}, openCodeOwnedKeys...), "provider")
	next["x-claude-code-managed"] = applyOpenCodePlugin(reg, existing, next, managed)
	body, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return false, fmt.Errorf("opencode: %w", err)
	}
	body = append(body, '\n')
	return commitFile(path, raw, body, existed, o, out, "opencode")
}

// renderOpenCodeCopilot renders opencode.json for opencode's own GitHub Copilot login: opencode already
// owns that provider block (it wrote it via `opencode auth login`), so this writes only model/small_model
// (sonnet/haiku) plus permission/mcp, drops any apiKey/provider wiring, and clears a stale harness-*
// provider entry a previous (pre-login) run of this tool left behind.
func renderOpenCodeCopilot(reg *providers.Registry, home string, o Options, out io.Writer) (bool, error) {
	mainID, smallID, ok := vendors.OpencodeCopilotModelIDs(reg)
	if !ok {
		fmt.Fprintln(out, "opencode: skipped: github-copilot has no sonnet/haiku model configured")
		return false, nil
	}
	path := opencodeJSONPath(home)
	raw, readErr := os.ReadFile(path)
	existed := readErr == nil
	var existing map[string]any
	if existed {
		_ = json.Unmarshal(raw, &existing)
	}
	next := map[string]any{}
	maps.Copy(next, existing)
	next["$schema"] = "https://opencode.ai/config.json"
	next["model"] = "github-copilot/" + mainID
	next["small_model"] = "github-copilot/" + smallID
	if pm, ok := next["provider"].(map[string]any); ok {
		for k := range pm {
			if strings.HasPrefix(k, "harness-") {
				delete(pm, k)
			}
		}
		if len(pm) == 0 {
			delete(next, "provider")
		} else {
			next["provider"] = pm
		}
	}
	permission, mcp, _ := vendors.OpenCodePermissionMCP() // mcpSkipped has no env channel on this render path
	if permission != nil {
		next["permission"] = permission
	} else {
		delete(next, "permission")
	}
	if len(mcp) > 0 {
		next["mcp"] = mcp
	} else {
		delete(next, "mcp")
	}
	managed := append([]string{}, openCodeCopilotOwnedKeys...)
	next["x-claude-code-managed"] = applyOpenCodePlugin(reg, existing, next, managed)
	body, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return false, fmt.Errorf("opencode: %w", err)
	}
	body = append(body, '\n')
	return commitFile(path, raw, body, existed, o, out, "opencode")
}

// applyOpenCodePlugin sets next["plugin"] from reg.OpencodePlugins and appends "plugin" to managed when
// non-empty. Empty leaves any existing plugin key untouched and unclaimed, except: if this tool wrote it
// on a previous run (named in existing's own x-claude-code-managed), the list going empty means drop it,
// the same way every other managed key is retired here.
func applyOpenCodePlugin(reg *providers.Registry, existing, next map[string]any, managed []string) []string {
	if len(reg.OpencodePlugins) > 0 {
		next["plugin"] = append([]string{}, reg.OpencodePlugins...)
		return append(managed, "plugin")
	}
	if wasManagedKey(existing, "plugin") {
		delete(next, "plugin")
	}
	return managed
}

// wasManagedKey reports whether key was named in existing's own x-claude-code-managed list.
func wasManagedKey(existing map[string]any, key string) bool {
	list, _ := existing["x-claude-code-managed"].([]any)
	for _, v := range list {
		if s, ok := v.(string); ok && s == key {
			return true
		}
	}
	return false
}

// openCodeDefaultTheme draws on the terminal's own colors, so opencode inherits the VS Code terminal background.
const openCodeDefaultTheme = "system"

// renderOpenCodeTUI seeds theme in ~/.config/opencode/tui.json (1.18+ moves it out of opencode.json); a set theme is kept.
func renderOpenCodeTUI(home string, o Options, out io.Writer) (bool, error) {
	path := filepath.Join(home, ".config", "opencode", "tui.json")
	raw, readErr := os.ReadFile(path)
	existed := readErr == nil
	cfg := map[string]any{}
	if existed {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			fmt.Fprintf(out, "opencode-tui: skipped: %s is not plain JSON: %v\n", path, err)
			return false, nil
		}
	}
	if _, ok := cfg["theme"]; ok {
		fmt.Fprintf(out, "opencode-tui: %s unchanged (theme already set)\n", path)
		return false, nil
	}
	next := map[string]any{"$schema": "https://opencode.ai/tui.json"}
	maps.Copy(next, cfg)
	next["theme"] = openCodeDefaultTheme
	body, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return false, fmt.Errorf("opencode-tui: %w", err)
	}
	return commitFile(path, raw, append(body, '\n'), existed, o, out, "opencode-tui")
}

// --- gemini: report-only. agy (the vendor's binary, name unchanged) has no known settings-file
// auth-type knob like the old gemini-cli's security.auth.selectedType, so there is nothing to render;
// auth is the cached Google login marker or GEMINI_API_KEY at launch time (see vendors.GeminiLoginPresent).

func reportGemini(sel providers.Selection, out io.Writer) (bool, error) {
	if sel.Provider.Auth.Type == providers.AuthPassthrough {
		fmt.Fprintln(out, "gemini: skipped: session provider is passthrough, not keyed")
		return false, nil
	}
	fmt.Fprintln(out, "gemini: no config render; agy has no settings-auth equivalent (launch-time env/login only)")
	return false, nil
}

// --- copilot: launch-time env only; generate reports the resolved route and writes nothing ---

func reportCopilot(reg *providers.Registry, sel providers.Selection, out io.Writer) (bool, error) {
	ctx := context.Background()
	target, note, err := vendors.ResolveTarget(reg, "copilot", sel, false)
	if err != nil {
		fmt.Fprintf(out, "copilot: skipped: %v\n", err)
		return false, nil
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if _, _, err := vendors.Configure(ctx, "copilot", target, ""); err != nil {
		fmt.Fprintf(out, "copilot: skipped: %v\n", err)
		return false, nil
	}
	fmt.Fprintf(out, "copilot: launch-time env only; route = %s (%s)\n", target.Provider.Name, target.Model)
	return false, nil
}
