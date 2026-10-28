package vendors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/copilotreg"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

// Seams for tests: model discovery and the router's session tokens.
var (
	discoverModels = copilotreg.Discover
	mintSession    = proxy.MintSessionToken
	revokeSession  = proxy.RevokeSessionToken
)

// CopilotRegistryBanner is the one-line launch notice; GitHub models are unreachable by construction in this mode.
const CopilotRegistryBanner = "copilot: GitHub models are unavailable in this mode"

// CopilotStripEnv is the inherited GitHub login/routing env every registry child (launch or SDK) must not see;
// COPILOT_PROVIDER_* is stripped by CopilotStripPrefix.
var CopilotStripEnv = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_ENTERPRISE_TOKEN", "COPILOT_GITHUB_TOKEN",
	"COPILOT_SDK_AUTH_TOKEN", "GH_HOST", "COPILOT_GH_HOST", "COPILOT_API_URL", "GH_CONFIG_DIR",
	"COPILOT_PROVIDERS_CONFIG",
}

// CopilotStripPrefix names the singular BYOK block (COPILOT_PROVIDER_*) a registry child must not inherit.
const CopilotStripPrefix = "COPILOT_PROVIDER_"

// CopilotPrivateHome is the private login-free copilot home: the env to set and the names to strip.
type CopilotPrivateHome struct {
	Env      []string
	Unset    []string // exact names; also strip anything with CopilotStripPrefix
	Warnings []string
}

// PrepareCopilotHome ensures and seeds the private home (skipped when dry), runs the login guard, and returns its
// env. Interactive launches and SDK ask runs share it, so sessions made by one resume in the other.
func PrepareCopilotHome(dry bool) (CopilotPrivateHome, error) {
	home, xdg := copilotRegistryHome(), copilotRegistryXDG()
	out := CopilotPrivateHome{
		Env: []string{
			"COPILOT_HOME=" + home, "XDG_CONFIG_HOME=" + xdg,
			"DBUS_SESSION_BUS_ADDRESS=", // no session bus, so no keyring: nothing to log in with
		},
		Unset: append(slices.Clone(CopilotStripEnv), "COPILOT_MODEL"),
	}
	if dry {
		return out, nil
	}
	var err error
	if out.Warnings, err = seedCopilotHome(copilotSourceHome(home), home, xdg); err != nil {
		return CopilotPrivateHome{}, err
	}
	return out, nil
}

// CopilotRegistry is one registry launch: the env to set, names to unset, argv prefix, and the minted token.
type CopilotRegistry struct {
	Env      []string
	Unset    []string
	Argv     []string
	Args     []string // the caller's args minus any --model (selection already applied)
	Warnings []string
	Token    string
}

// Revoke drops the launch's session token; a launch that never minted one is a no-op.
func (c CopilotRegistry) Revoke() {
	if c.Token != "" {
		_ = revokeSession(context.Background(), c.Token)
	}
}

// UsesCopilotRegistry reports whether t is a Databricks row whose copilot launch runs on the registry.
func UsesCopilotRegistry(ctx context.Context, t providers.Target) bool {
	return !isDirect(ctx) && t.Provider != nil && t.Provider.Auth.Login == "" &&
		t.Provider.Preset == providers.PresetDatabricks
}

// ConfigureCopilotRegistry builds the interactive registry launch for Databricks target t. Every error is fatal to
// the caller: nothing here may fall back to a plain copilot, which would run on its own GitHub login. dry skips
// minting, writing and the private home (--print-env).
func ConfigureCopilotRegistry(
	ctx context.Context, reg *providers.Registry, t providers.Target, args []string, dry bool,
) (CopilotRegistry, error) {
	var out CopilotRegistry
	p := t.Provider
	requested, rest := splitModelFlags(args)
	eps, err := discoverModels(ctx, reg, p)
	if err != nil {
		return out, fmt.Errorf("copilot: list models for profile %s: %w", p.Name, err)
	}
	rb, err := ensureRouter(ctx)
	if err != nil {
		return out, fmt.Errorf("copilot: router: %w", err)
	}
	rp := *p
	rp.Headers = map[string]string{proxy.AgentHeader: "copilot"} // the router adds the provider's own headers upstream
	r := copilotreg.Build(p.Name, &rp, eps, rb)
	ref, err := r.Select(copilotreg.Choice{Requested: requested, Pin: providers.Pin(providers.PinCopilotModel)})
	if err != nil {
		return out, fmt.Errorf("copilot: %w", err)
	}
	pid := os.Getpid()
	file := ""
	if dry {
		dir, err := copilotreg.RuntimeDir()
		if err != nil {
			return out, fmt.Errorf("copilot: %w", err)
		}
		file = copilotreg.RegistryFile(dir, pid)
	} else if file, out.Token, err = prepareRegistry(ctx, r, p.Name, pid); err != nil {
		out.Revoke()
		out.Token = ""
		return out, err
	}
	ph, err := PrepareCopilotHome(dry)
	if err != nil {
		out.Revoke()
		out.Token = ""
		return out, err
	}
	// OFFLINE removes /login but needs the registry file at startup; the SDK path has none, so it stays here.
	out.Env = append([]string{"COPILOT_PROVIDERS_CONFIG=" + file, "COPILOT_OFFLINE=true"}, ph.Env...)
	out.Unset, out.Warnings = ph.Unset, ph.Warnings
	out.Argv = append([]string{"--model", ref}, copilotPinArgv()...)
	out.Args = rest
	return out, nil
}

// prepareRegistry mints the token and writes the registry file; on error the caller revokes.
func prepareRegistry(ctx context.Context, r copilotreg.Registry, profile string, pid int,
) (file, token string, err error) {
	if token, err = mintSession(ctx, profile, pid); err != nil {
		return "", "", fmt.Errorf("copilot: session token for profile %s: %w", profile, err)
	}
	data, err := copilotreg.Render(r, token)
	if err != nil {
		return "", token, fmt.Errorf("copilot: render registry: %w", err)
	}
	if file, err = copilotreg.WriteRegistry(pid, data); err != nil {
		return "", token, fmt.Errorf("copilot: %w", err)
	}
	return file, token, nil
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

// copilotSeedLinks are linked from the real home; config.json (the GitHub login), session state, the history
// DB, ucode.env (a bearer token) and logs are never carried over.
var copilotSeedLinks = []string{
	"agents", "skills", "hooks", "plugins", "installed-plugins", "copilot-instructions.md", "mcp-config.json",
	"permissions-config.json",
}

// seedCopilotHome creates the private home and xdg dirs (0700), refreshes the links, copies settings.json without
// its model key, and refuses a home that holds a GitHub login.
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
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("copilot: refresh %s: %w", dst, err)
		}
	}
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	if err := os.Symlink(src, dst); err != nil {
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
	var lines []string
	for l := range strings.SplitSeq(string(raw), "\n") { // config.json opens with // comment lines
		if !strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines = append(lines, l)
		}
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &cfg); err != nil {
		return fmt.Errorf("copilot: %s is unreadable (%v); delete it to launch in registry mode", file, err)
	}
	for _, k := range []string{"loggedInUsers", "lastLoggedInUser", "copilotTokens", "authTokens"} {
		if v := strings.TrimSpace(string(cfg[k])); v != "" && v != "null" && v != "[]" && v != "{}" {
			return fmt.Errorf("copilot: %s holds a GitHub login (%s), which would let models fall through to "+
				"GitHub; delete the file (and run no /login in registry mode)", file, k)
		}
	}
	return nil
}

// splitModelFlags removes every --model <v> / --model=<v> from args and returns the values in order.
func splitModelFlags(args []string) (models, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--model" && i+1 < len(args):
			models = append(models, args[i+1])
			i++
		case strings.HasPrefix(a, "--model="):
			models = append(models, strings.TrimPrefix(a, "--model="))
		default:
			rest = append(rest, a)
		}
	}
	return models, rest
}
