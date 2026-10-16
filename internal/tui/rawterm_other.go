//go:build !linux && !darwin

package tui

import "errors"

// termState is a placeholder off unix: no raw mode, every interactive path falls back to line prompts.
type termState struct{}

var errNoTTY = errors.New("tui: raw terminal mode is not supported on this platform")

func isTerminal(int) bool              { return false }
func makeRaw(int) (*termState, error)  { return nil, errNoTTY }
func restore(int, *termState) error    { return nil }
func getSize(int) (int, int, error)    { return 0, 0, errNoTTY }
func readPassword(int) ([]byte, error) { return nil, errNoTTY }
