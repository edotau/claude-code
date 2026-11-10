package browser

import (
	"errors"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

// action is one repair step Setup took (or, with Env.DryRun, planned).
type action struct {
	Kind        string   `json:"kind"` // missing-host|enable-extension|install-extension|noop
	Description string   `json:"description"`
	Command     []string `json:"command,omitempty"`
	Executed    bool     `json:"executed"`
	Error       string   `json:"error,omitempty"`
}

// Actions is Setup's result for one browser.
type Actions struct {
	Browser string   `json:"browser"`
	Items   []action `json:"items"`
}

// Setup repairs only what the harness owns (never Claude Code's host manifest) and opens the extension install page.
func Setup(env Env, name string) (Actions, error) {
	resolved, err := resolveSetupName(env, name)
	if err != nil {
		return Actions{}, err
	}
	s, ok := specByName(resolved)
	if !ok {
		return Actions{}, fmt.Errorf("browser: unknown browser %q", name)
	}
	if s.family != familyChromium {
		return Actions{}, fmt.Errorf("browser: %s has no extension control path — see tier 3 (safaridriver/geckodriver)",
			s.displayName)
	}
	b := detectOne(env, s)
	if !b.Installed {
		return Actions{}, fmt.Errorf("browser: %s is not installed", s.displayName)
	}

	if b.Host == stateUnsupportedWSL {
		return setupWSL(env, b), nil
	}
	actions := Actions{Browser: b.Name}
	switch b.Paths.Extension {
	case stateNoHost:
		actions.Items = append(actions.Items, missingHostAction(b))
	case stateReady:
		ready := action{Kind: "noop", Description: b.DisplayName + " is already ready.", Executed: true}
		actions.Items = append(actions.Items, ready)
	case stateDisabled:
		actions.Items = append(actions.Items, enableExtensionAction(b))
	default: // no-extension
		actions.Items = append(actions.Items, installExtensionAction(env, b))
	}
	return actions, nil
}

// resolveSetupName turns "default" into the resolved default's registry name; any other name passes through.
func resolveSetupName(env Env, name string) (string, error) {
	if name != "default" {
		return name, nil
	}
	b, err := Default(env)
	if err != nil && !errors.Is(err, errDefaultUnsupported) {
		return "", err
	}
	if b.Name == "" {
		return "", errors.New("browser: no resolvable system default")
	}
	return b.Name, nil
}

func missingHostAction(b Browser) action {
	return action{
		Kind: "missing-host",
		Description: fmt.Sprintf(
			"Claude Code owns the native-messaging host for %s; "+
				"run `claude --chrome` (or reinstall Claude Code) to register it.",
			b.DisplayName),
	}
}

func enableExtensionAction(b Browser) action {
	return action{
		Kind: "enable-extension",
		Description: fmt.Sprintf("The extension is installed but disabled in %s — enable it from its extensions page.",
			b.DisplayName),
	}
}

// installExtensionAction opens the Web Store page: macOS `open -b <bundle>` targets the browser, else the default.
func installExtensionAction(env Env, b Browser) action {
	cmd := []string{"open", "-b", b.BundleID, webStoreURL}
	if env.goos() != "darwin" {
		cmd = []string{proc.Opener(), webStoreURL}
	}
	a := action{
		Kind: "install-extension",
		Description: fmt.Sprintf(
			"Opening the Chrome Web Store install page in %s — sign in and click \"Add to %s\".",
			b.DisplayName, b.DisplayName),
		Command: cmd,
	}
	if env.DryRun {
		return a
	}
	out, err := env.run(cmd[0], cmd[1:]...)
	a.Executed = err == nil
	if err != nil {
		a.Error = strings.TrimSpace(out + " " + err.Error())
	}
	return a
}
