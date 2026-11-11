package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
)

// copilotGOOS is the OS the registry guard checks; a seam for tests.
var copilotGOOS = runtime.GOOS

// StubGOOS makes the registry guard see goos, for tests in other packages; restore undoes it.
func StubGOOS(goos string) (restore func()) {
	orig := copilotGOOS
	copilotGOOS = goos
	return func() { copilotGOOS = orig }
}

// errCopilotRegistryDarwin refuses the registry on macOS (Keychain login unhidden); fits FallbackReason's 200 runes.
var errCopilotRegistryDarwin = errors.New(
	"copilot: the Databricks model registry is unsupported on macOS (copilot's Keychain GitHub login is not hidden); " +
		"use --direct")

func checkCopilotRegistryOS() error {
	if copilotGOOS == "darwin" {
		return errCopilotRegistryDarwin
	}
	return nil
}

// copilotStripEnv is the inherited GitHub login/routing env a registry child must not see (plus CopilotStripPrefix).
var copilotStripEnv = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_ENTERPRISE_TOKEN", "COPILOT_GITHUB_TOKEN",
	"COPILOT_SDK_AUTH_TOKEN", "GH_HOST", "COPILOT_GH_HOST", "COPILOT_API_URL", "GH_CONFIG_DIR",
	proc.EnvCopilotProvidersConfig,
}

// copilotStripPrefix names the singular BYOK block (COPILOT_PROVIDER_*) a registry child must not inherit.
const copilotStripPrefix = proc.CopilotProviderPrefix

// copilotPrivateHome is the private login-free copilot home: the env to set and the names to strip.
type copilotPrivateHome struct {
	Env      []string
	Unset    []string // exact names; also strip anything with CopilotStripPrefix
	Warnings []string
}

// prepareCopilotHome seeds the private home (not when dry), guards logins, returns its env; launch and ask share it.
func prepareCopilotHome(dry bool) (copilotPrivateHome, error) {
	if err := checkCopilotRegistryOS(); err != nil {
		return copilotPrivateHome{}, err
	}
	home, xdg := copilotRegistryHome(), copilotRegistryXDG()
	out := copilotPrivateHome{
		Env: []string{
			"COPILOT_HOME=" + home, "XDG_CONFIG_HOME=" + xdg,
			"DBUS_SESSION_BUS_ADDRESS=", // no session bus, so no keyring: nothing to log in with
		},
		Unset: append(slices.Clone(copilotStripEnv), "COPILOT_MODEL"),
	}
	if dry {
		return out, nil
	}
	// Concurrent launches share the home; initialise it one process at a time.
	err := paths.WithFileLock(filepath.Join(filepath.Dir(home), "registry-home.init"), func() error {
		var serr error
		out.Warnings, serr = seedCopilotHome(copilotSourceHome(home), home, xdg)
		return serr
	})
	if err != nil {
		return copilotPrivateHome{}, err
	}
	return out, nil
}

func copilotRegistryHome() string {
	return filepath.Join(paths.ConfigDir(), "state", "copilot", "registry-home")
}

func copilotRegistryXDG() string {
	return filepath.Join(paths.ConfigDir(), "state", "copilot", "registry-xdg")
}

// copilotSourceHome is the user's real copilot home; a COPILOT_HOME inherited from a nested launch is not it.
func copilotSourceHome(registryHome string) string {
	if h := os.Getenv("COPILOT_HOME"); h != "" && filepath.Clean(h) != registryHome {
		return h
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(u, ".copilot")
}

// copilotSeedLinks are linked from the real home; the GitHub login, session state, history, ucode.env and logs are not.
var copilotSeedLinks = []string{
	"agents", "skills", "hooks", "plugins", "installed-plugins", "copilot-instructions.md", "mcp-config.json",
	"permissions-config.json",
}

// seedCopilotHome makes the 0700 home and xdg, relinks, copies settings.json minus its model key, refuses a login.
func seedCopilotHome(src, home, xdg string) (warnings []string, err error) {
	for _, d := range []string{home, xdg} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("copilot: private home: %w", err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, fmt.Errorf("copilot: private home: %w", err)
		}
	}
	if err := checkNoGitHubLogin(filepath.Join(home, "config.json")); err != nil {
		return nil, err
	}
	if src == "" {
		return []string{"copilot: no real copilot home to seed from; agents, skills and settings are not carried over"}, nil
	}
	for _, name := range copilotSeedLinks {
		if err := refreshLink(filepath.Join(src, name), filepath.Join(home, name)); err != nil {
			return nil, err
		}
	}
	if err := copySettings(filepath.Join(src, "settings.json"), filepath.Join(home, "settings.json")); err != nil {
		warnings = append(warnings, err.Error())
	}
	return warnings, nil
}

// refreshLink makes dst a symlink to src, or removes a stale link when src is gone; a real file at dst is kept.
func refreshLink(src, dst string) error {
	if fi, err := os.Lstat(dst); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return nil
		}
	}
	if _, err := os.Stat(src); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("copilot: stat %s: %w", src, err)
		}
		if rerr := os.Remove(dst); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return fmt.Errorf("copilot: refresh %s: %w", dst, rerr)
		}
		return nil
	}
	if err := paths.SwapSymlink(src, dst); err != nil {
		return fmt.Errorf("copilot: link %s: %w", dst, err)
	}
	return nil
}

// copySettings copies settings.json without "model", so a registry pick never rewrites the user's default.
func copySettings(src, dst string) error {
	raw, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(dst)
		return nil
	}
	if err != nil {
		_ = os.Remove(dst) // a stale copy must not outlive a failed read
		return fmt.Errorf("copilot: settings.json not carried over: %w", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("copilot: settings.json not carried over: %w", err)
	}
	delete(m, "model")
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("copilot: settings.json not carried over: %w", err)
	}
	return paths.AtomicWrite(dst, out, 0o600)
}

// checkNoGitHubLogin fails when the private home's config.json holds a logged-in GitHub user or token.
func checkNoGitHubLogin(file string) error {
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("copilot: read %s: %w", file, err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(commentRE.ReplaceAll(raw, nil), &cfg); err != nil {
		return fmt.Errorf("copilot: %s is unreadable (%v); delete it to launch in registry mode", file, err)
	}
	for _, k := range []string{"loggedInUsers", "lastLoggedInUser", "copilotTokens", "authTokens"} {
		if jsonValuePresent(cfg[k]) {
			return fmt.Errorf("copilot: %s holds a GitHub login (%s), which would let models fall through to "+
				"GitHub; delete the file (and run no /login in registry mode)", file, k)
		}
	}
	return nil
}

// jsonValuePresent reports whether v is set to something other than null or an empty array/object (any spacing).
func jsonValuePresent(v json.RawMessage) bool {
	if len(v) == 0 {
		return false
	}
	var val any
	if err := json.Unmarshal(v, &val); err != nil {
		return true // unparseable but set: keep refusing, as before
	}
	switch t := val.(type) {
	case nil:
		return false
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

func hooksPath(home string) string {
	return filepath.Join(home, ".copilot", "hooks", "claude-code-harness.json")
}

func settingsPath(home string) string { return filepath.Join(home, ".copilot", "settings.json") }

// Copilot hooks load from the user directory; plugin preToolUse hooks do not fire.
func renderCopilot(reg *providers.Registry, sel providers.Selection, home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	body, err := CopilotJSON()
	if err != nil {
		return false, err
	}
	drift, err := agentconfig.RenderHooksFile(hooksPath(home), body, mergeCopilotHooks, o, out, "copilot-hooks")
	if err != nil {
		return drift, err
	}
	sd, err := renderCopilotSettings(home, o, out)
	if err != nil {
		return drift, err
	}
	drift = drift || sd
	ctx := context.Background()
	target, note, err := copilotSchema().ResolveTarget(reg, sel, false)
	if err != nil {
		fmt.Fprintf(out, "copilot: skipped: %v\n", err)
		return drift, nil
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if _, _, err := copilotSchema().Configure(ctx, target); err != nil {
		fmt.Fprintf(out, "copilot: skipped: %v\n", err)
		return drift, nil
	}
	fmt.Fprintf(out, "copilot: launch-time env only; route = %s (%s)\n", target.Provider.Name, target.Model)
	return drift, nil
}

// extraDefaultPermissionMode is the record extras key copilot's settings.json takes as defaultPermissionMode.
const extraDefaultPermissionMode = "defaultPermissionMode"

// renderCopilotSettings merges the record's extras.defaultPermissionMode into ~/.copilot/settings.json,
// keeping every other key untouched; a non-JSON existing file is skipped, never clobbered.
func renderCopilotSettings(home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	rec, err := agentconfig.Settings("copilot")
	if err != nil {
		fmt.Fprintf(out, "copilot-settings: skipped: %v\n", err)
		return false, nil
	}
	mode := rec.Extras[extraDefaultPermissionMode]
	if mode == "" {
		return false, nil
	}
	return agentconfig.CommitJSONObject(settingsPath(home), o, out, "copilot-settings", func(obj map[string]any, _ bool) bool {
		obj["defaultPermissionMode"] = mode
		return true
	})
}

var agentEvents = map[string]string{
	settings.EventPreToolUse:       "preToolUse",
	settings.EventPostToolUse:      "postToolUse",
	settings.EventUserPromptSubmit: "userPromptSubmitted",
	settings.EventStop:             "agentStop",
	settings.EventSessionEnd:       "sessionEnd",
}

// CopilotJSON renders user-level hooks; advisory failures must never deny a tool.
func CopilotJSON() ([]byte, error) {
	type entry struct {
		Type       string `json:"type"`
		Matcher    string `json:"matcher"`
		Bash       string `json:"bash"`
		TimeoutSec int    `json:"timeoutSec"`
	}
	events := map[string][]entry{}
	for _, s := range settings.SpecsFor("copilot", "native") {
		event := agentEvents[s.Event]
		if event == "" {
			continue
		}
		command := s.Command()
		if !s.Blocks {
			command += " || true"
		}
		matcher := s.Matcher
		if matcher == "*" {
			matcher = ""
		} else if matcher != "" {
			matcher = strings.NewReplacer("Write", "Write|create", "Read", "Read|view").Replace(matcher)
			matcher = copilotMatcher(matcher)
		}
		events[event] = append(events[event], entry{Type: "command", Matcher: matcher, Bash: command, TimeoutSec: s.Seconds()})
	}
	return json.MarshalIndent(map[string]any{"version": 1, "hooks": events}, "", "  ")
}

// copilotMatcher lists each tool name as given and lowercased: Copilot rejects inline flags like (?i) and anchors.
func copilotMatcher(m string) string {
	var out []string
	seen := map[string]bool{}
	for n := range strings.SplitSeq(m, "|") {
		for _, v := range []string{n, strings.ToLower(n)} {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return strings.Join(out, "|")
}

// mergeCopilotHooks is mergeCodexHooks for Copilot's flat (non-grouped) hook entries and its top-level
// {"version", "hooks"} shape: each entry is either kept whole, byte-identical (foreign) or dropped whole
// (harness-owned, by its "bash" command prefix); every other top-level key survives untouched.
func mergeCopilotHooks(existingRaw, body []byte) ([]byte, error) {
	next, renderedTop, err := agentconfig.MergeHooksTop(existingRaw, body, classifyCopilotEntry)
	if err != nil {
		return nil, err
	}
	var version int
	_ = json.Unmarshal(renderedTop["version"], &version) // absent/malformed rendered version: fall through to 1
	if version == 0 {
		version = 1
	}
	versionRaw, err := textutil.MarshalNoEscape(version)
	if err != nil {
		return nil, err
	}
	next["version"] = versionRaw
	return textutil.MarshalIndentNoEscape(next)
}

// classifyCopilotEntry reports an entry's foreign remainder (nil when harness-owned, by its "bash"
// command prefix via IsHarness) and whether it was harness-owned; a foreign entry is returned
// byte-identical so powershell/cwd/env and any other field survive untouched.
func classifyCopilotEntry(e json.RawMessage) (*json.RawMessage, bool) {
	var id struct {
		Bash string `json:"bash"`
	}
	if err := json.Unmarshal(e, &id); err == nil && settings.IsHarness(id.Bash) {
		return nil, true
	}
	return &e, false
}
