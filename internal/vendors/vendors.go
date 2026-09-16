// Package vendors wires third-party agent CLIs (codex, gemini, opencode, copilot) to a provider target,
// shared by the interactive `claude-code run` launch and the headless agent runners.
package vendors

import (
	"context"
	"errors"

	"github.com/edotau/claude-code/internal/providers"
)

// STUB: workstream D replaces this file; signatures are the contract the launcher codes against.

// Spec describes one vendor CLI.
type Spec struct {
	Name    string // codex | gemini | opencode | copilot
	Binary  string // executable name on PATH
	Dialect string // provider route the CLI speaks (providers.Dialect*)
}

// Lookup returns the vendor spec by name.
func Lookup(name string) (Spec, bool) { return Spec{}, false }

// Configure returns extra env (KEY=VALUE) and leading argv that point the vendor CLI at target.
// headlessPrompt != "" asks for the non-interactive form (the runner path).
func Configure(ctx context.Context, name string, target providers.Target, headlessPrompt string) (env, argv []string, err error) {
	return nil, nil, errors.New("vendors: not implemented")
}
