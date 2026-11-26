package browser

import (
	"errors"
	"fmt"
	"strings"
)

// action is one repair step Setup took (or, with Env.DryRun, planned).
type action struct {
	Kind        string   `json:"kind"` // missing-host|enable-extension|install-extension|start-cdp|register-mcp|noop
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
		return Actions{}, fmt.Errorf("browser: %s has no Claude extension; WebDriver-based control is not built",
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

func missingHostAction(b browser) action {
	return action{Kind: "missing-host", Description: hostFix(b)}
}

func enableExtensionAction(b browser) action {
	return action{Kind: "enable-extension", Description: enableFix(b)}
}

// installExtensionAction opens the Web Store page in the named browser: `open -b` (macOS) or openCandidates (Linux).
func installExtensionAction(env Env, b browser) action {
	var cands [][]string
	if env.goos() == "darwin" {
		cands = [][]string{{"open", "-b", b.BundleID, webStoreURL}}
	} else if s, ok := specByName(b.Name); ok {
		cands = openCandidates(env, s, webStoreURL)
	}
	where := b.DisplayName
	var cmd []string
	if len(cands) > 0 {
		cmd = cands[0]
		if cmd[0] == "xdg-open" {
			where = "the default browser"
		}
	}
	a := action{
		Kind: "install-extension",
		Description: fmt.Sprintf(
			"Opening the Chrome Web Store install page in %s — sign in and click \"Add to %s\".",
			where, b.DisplayName),
		Command: cmd,
	}
	if env.DryRun {
		return a
	}
	out, err := runCandidates(env, cands)
	a.Executed = err == nil
	if err != nil {
		a.Error = strings.TrimSpace(out + " " + err.Error())
	}
	return a
}
