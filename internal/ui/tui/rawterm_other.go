//go:build !linux && !darwin

package tui

import "errors"

// termState is a placeholder off unix: no raw mode, every interactive path falls back to line prompts.
type termState struct{}

var errNoTTY = errors.New("tui: raw terminal mode is not supported on this platform; run on linux or macOS, or pipe the input on stdin")

func makeRaw(int) (*termState, error)                { return nil, errNoTTY }
func restore(int, *termState) error                  { return nil }
func readPassword(int, func([]byte)) ([]byte, error) { return nil, errNoTTY }
