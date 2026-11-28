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

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/driver/copilotauth"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/settings"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// copilotGOOS is the OS the registry guard checks; a seam for tests.
var copilotGOOS = runtime.GOOS

// StubGOOS makes the registry guard see goos, for tests in other packages; restore undoes it.
func StubGOOS(goos string) (restore func()) {
	orig := copilotGOOS
	copilotGOOS = goos
	return func() { copilotGOOS = orig }
}

// copilotStripEnv is the inherited GitHub login/routing env a registry child must not see.
var copilotStripEnv = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_ENTERPRISE_TOKEN", "COPILOT_GITHUB_TOKEN",
	"COPILOT_SDK_AUTH_TOKEN", "GH_HOST", "COPILOT_GH_HOST", "COPILOT_API_URL", "GH_CONFIG_DIR",
	proc.EnvCopilotProvidersConfig,
}

// copilotPrivateHome is the private login-free copilot home: the env to set and the names to strip.
type copilotPrivateHome struct {
	Env proc.Values
	// exact names; also strip anything with proc.CopilotProviderPrefix
	Unset    []string
	Warnings []string
}

// prepareCopilotHome seeds the private home (not when dry), guards logins, returns its env; launch and ask share it.
func prepareCopilotHome(dry bool) (copilotPrivateHome, error) {
	home, xdg := copilotRegistryHome(), copilotRegistryXDG()
	out := copilotPrivateHome{
		Env: proc.Values{
			"COPILOT_HOME": home, "XDG_CONFIG_HOME": xdg,
			// no session bus, so no keyring: nothing to log in with
			"DBUS_SESSION_BUS_ADDRESS": "",
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
		// already pointing at src: no swap, and the stat below is only needed to drop a stale link
		if cur, rerr := os.Readlink(dst); rerr == nil && cur == src {
			if _, serr := os.Stat(src); serr == nil {
				return nil
			}
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
		return removeStale(dst)
	}
	if err != nil {
		return notCarried(err, removeStale(dst))
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return notCarried(err, removeStale(dst))
	}
	delete(m, "model")
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("copilot: settings.json not carried over: %w", err)
	}
	_, err = paths.WriteIfChanged(dst, out, 0o600)
	return err
}

// removeStale deletes a stale settings copy; a failed remove is an error, since the copy may carry a model key.
func removeStale(dst string) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("copilot: stale %s not removed: %w", dst, err)
	}
	return nil
}

func notCarried(cause, rmErr error) error {
	return errors.Join(fmt.Errorf("copilot: settings.json not carried over: %w", cause), rmErr)
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
	if err := json.Unmarshal(copilotauth.StripComments(raw), &cfg); err != nil {
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
		// unparseable but set: keep refusing, as before
		return true
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
	return paths.VendorPath(home, filepath.Join(".copilot", "hooks", "claude-code-harness.json"))
}

func settingsPath(home string) string {
	return paths.VendorPath(home, filepath.Join(".copilot", "settings.json"))
}

func mcpConfigPath(home string) string {
	return paths.VendorPath(home, filepath.Join(".copilot", "mcp-config.json"))
}

// MsgMCPName is the harness MCP server (agents mcp) copilot gets, interactive and headless.
const MsgMCPName = "harness"

// MsgMCPCommand is the msg server's argv: the absolute harness binary, then its args; the sender is copilot.
func MsgMCPCommand() []string {
	return []string{filepath.Join(paths.BinDir(), "claude-code"), "agents", "mcp", "--from", "copilot"}
}

// Copilot hooks load from the user directory; plugin preToolUse hooks do not fire.
func renderCopilot(ctx context.Context, reg *providers.Registry, sel providers.Selection, home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
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
	md, err := renderCopilotMCP(home, o, out)
	if err != nil {
		return drift, err
	}
	drift = drift || sd || md
	target, note, err := row.ResolveTarget(ctx, agent.RunOptions{}, reg, sel, false)
	if err != nil {
		fmt.Fprintf(out, "copilot: skipped: %v\n", err)
		return drift, nil
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if _, _, err := row.Configure(ctx, target); err != nil {
		fmt.Fprintf(out, "copilot: skipped: %v\n", err)
		return drift, nil
	}
	fmt.Fprintf(out, "copilot: launch-time env only; route = %s (%s)\n", target.Provider.Name, target.Model)
	return drift, nil
}

// extraDefaultPermissionMode is the record extras key copilot's settings.json takes as defaultPermissionMode.
const extraDefaultPermissionMode = "defaultPermissionMode"

// renderCopilotSettings merges the permission mode and two absent off switches into settings.json; non-JSON is skipped.
func renderCopilotSettings(home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	rec, err := agentconfig.Settings("copilot")
	if err != nil {
		fmt.Fprintf(out, "copilot-settings: skipped: %v\n", err)
		return false, nil
	}
	mode := rec.Extras[extraDefaultPermissionMode]
	return agentconfig.CommitJSONObject(settingsPath(home), o, out, "copilot-settings", func(obj map[string]any, _ bool) bool {
		if mode != "" {
			obj["defaultPermissionMode"] = mode
		}
		for _, k := range []string{"includeCoAuthoredBy", "memory"} { // the owner's own value, true or false, stays
			if _, set := obj[k]; !set {
				obj[k] = false // no footer the harness blocks on every commit, no second server-side memory store
			}
		}
		return true
	})
}

// renderCopilotMCP merges the msg server into mcp-config.json; the owner's other servers stay as they are.
func renderCopilotMCP(home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	argv := MsgMCPCommand()
	entry := map[string]any{"type": "local", "command": argv[0], "args": argv[1:], "tools": []string{"*"}}
	path := mcpConfigPath(home)
	return agentconfig.CommitJSONObject(path, o, out, "copilot-mcp", func(obj map[string]any, _ bool) bool {
		servers, ok := obj["mcpServers"].(map[string]any)
		if _, set := obj["mcpServers"]; set && !ok {
			fmt.Fprintf(out, "copilot-mcp: skipped: %s has a non-object mcpServers\n", path)
			return false
		}
		if servers == nil {
			servers = map[string]any{}
		}
		if cur, set := servers[MsgMCPName]; set && !sameCopilotMCP(cur, entry) {
			fmt.Fprintf(out, "copilot-mcp: skipped: %s declares its own %s server\n", path, MsgMCPName)
			return false
		}
		legacy := map[string]any{"type": "local", "command": argv[0], "args": []string{"agents", "msg", "--mcp", "--from", "copilot"}, "tools": []string{"*"}}
		if old, ok := servers["msg"]; ok && sameCopilotMCP(old, legacy) {
			delete(servers, "msg") // the server's name before it carried more than messages
		}
		servers[MsgMCPName] = entry
		obj["mcpServers"] = servers
		return true
	})
}

// A command basename alone proves no ownership: preserve custom args, fields and malformed entries.
func sameCopilotMCP(a, b any) bool {
	x, ex := json.Marshal(a)
	y, ey := json.Marshal(b)
	return ex == nil && ey == nil && agentconfig.SameJSONObject(x, y)
}

var agentEvents = map[string]string{
	settings.EventSessionStart:     "sessionStart",
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

// mergeCopilotHooks keeps foreign entries byte-identical and drops harness-owned ones by "bash" prefix.
func mergeCopilotHooks(existingRaw, body []byte) ([]byte, error) {
	next, renderedTop, err := agentconfig.MergeHooksTop(existingRaw, body, classifyCopilotEntry)
	if err != nil {
		return nil, err
	}
	var version int
	// absent/malformed rendered version: fall through to 1
	_ = json.Unmarshal(renderedTop["version"], &version)
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

// classifyCopilotEntry returns a foreign entry byte-identical (nil if harness-owned) and the owned flag.
func classifyCopilotEntry(e json.RawMessage) (*json.RawMessage, bool) {
	var id struct {
		Bash string `json:"bash"`
	}
	if err := json.Unmarshal(e, &id); err == nil && settings.IsHarness(id.Bash) {
		return nil, true
	}
	return &e, false
}
