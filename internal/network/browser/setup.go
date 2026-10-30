package browser

import (
	"errors"
	"fmt"
	"strings"
)

// Action is one repair step Setup took (or, with Env.DryRun, planned).
type Action struct {
	Kind        string   `json:"kind"` // missing-host|enable-extension|install-extension|noop
	Description string   `json:"description"`
	Command     []string `json:"command,omitempty"`
	Executed    bool     `json:"executed"`
	Error       string   `json:"error,omitempty"`
}

// Actions is Setup's result for one browser.
type Actions struct {
	Browser string   `json:"browser"`
	Items   []Action `json:"items"`
}

// Setup repairs only what the harness may own for the named browser (or "default", resolved via
// Default): it never writes Claude Code's own native-messaging host manifest — Claude Code is the
// only writer of that file, so a missing one gets a "run `claude --chrome`" action instead. The one
// thing it does execute (through Env.run, skipped when Env.DryRun) is opening the Chrome Web Store
// install page in the target browser; installing/enabling the extension itself still needs the
// user's click and sign-in.
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
		return Actions{}, fmt.Errorf("browser: %s has no extension control path — see tier 3 (safaridriver/geckodriver)", s.displayName)
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
		actions.Items = append(actions.Items, Action{Kind: "noop", Description: b.DisplayName + " is already ready.", Executed: true})
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
	if err != nil && !errors.Is(err, ErrDefaultUnsupported) {
		return "", err
	}
	if b.Name == "" {
		return "", errors.New("browser: no resolvable system default")
	}
	return b.Name, nil
}

func missingHostAction(b Browser) Action {
	return Action{
		Kind: "missing-host",
		Description: fmt.Sprintf(
			"Claude Code owns the native-messaging host for %s; run `claude --chrome` (or reinstall Claude Code) to register it.",
			b.DisplayName),
	}
}

func enableExtensionAction(b Browser) Action {
	return Action{
		Kind:        "enable-extension",
		Description: fmt.Sprintf("The extension is installed but disabled in %s — enable it from its extensions page.", b.DisplayName),
	}
}

// installExtensionAction opens the Web Store page in the target browser (macOS: `open -b <bundle>`,
// so it launches in that specific browser rather than the system default; Linux: `xdg-open`, which
// has no per-browser targeting equivalent).
func installExtensionAction(env Env, b Browser) Action {
	cmd := []string{"open", "-b", b.BundleID, webStoreURL}
	if env.goos() != "darwin" {
		cmd = []string{"xdg-open", webStoreURL}
	}
	a := Action{
		Kind: "install-extension",
		Description: fmt.Sprintf(
			"Opening the Chrome Web Store install page in %s — sign in and click \"Add to %s\".", b.DisplayName, b.DisplayName),
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
