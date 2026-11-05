package claude

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// AnthropicSecret reads the mode's vault key; when it is missing and a terminal is attached, offers to store it.
func AnthropicSecret(o AnthropicOptions, store func(name string) int) (string, error) {
	if v, err := readAnthropicSecret(o); err != nil {
		return "", fmt.Errorf("%s: %w", o.Name, err)
	} else if v != "" {
		return v, nil
	}
	missing := fmt.Errorf("%s is not in the vault; store it with `claude-code vault set %s`", o.Name, o.Name)
	if o.PrintEnv || !tui.StdinIsTerminal() || !ansi.IsTerminal(os.Stderr) {
		return "", missing
	}
	if !tui.Confirm(o.Name+" is not in the vault — set it now?", true) {
		return "", missing
	}
	if err := storeAnthropicSecret(o, store); err != nil {
		return "", err
	}
	if v, err := readAnthropicSecret(o); err != nil {
		return "", fmt.Errorf("%s: %w", o.Name, err)
	} else if v != "" {
		return v, nil
	}
	return "", missing
}

func readAnthropicSecret(o AnthropicOptions) (string, error) {
	if o.Mode == AuthOAuth {
		return providers.StoredSecret(o.Name), nil
	}
	v, _, err := vault.EffectiveSecret(o.Name)
	return v, err
}

func storeAnthropicSecret(o AnthropicOptions, store func(name string) int) error {
	if o.Mode == AuthOAuth && tui.Confirm("Run `claude setup-token` first to mint one?", true) {
		runSetupToken()
	}
	if code := store(o.Name); code != 0 {
		return fmt.Errorf("%s was not stored", o.Name)
	}
	return nil
}

// runSetupToken runs `claude setup-token` on the inherited terminal; a failure only means the user pastes a token.
func runSetupToken() {
	bin, err := ClaudeBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-code:", err)
		return
	}
	cmd := exec.CommandContext(context.Background(), bin, "setup-token")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "claude-code: claude setup-token:", err)
	}
}
