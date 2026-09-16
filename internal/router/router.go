// Package router is the loopback model router Claude Code talks to when a session needs translation,
// cross-provider slots, or failover.
package router

import (
	"context"
	"errors"
)

// STUB: workstream B replaces this file; signatures are the contract the launcher codes against.

// Ensure starts the router daemon if it is not already healthy and returns its base URL (http://127.0.0.1:<port>).
func Ensure(ctx context.Context) (string, error) { return "", errors.New("router: not implemented") }

// ClientSecret returns the per-install secret clients must present (created 0600 on first use).
func ClientSecret() (string, error) { return "", errors.New("router: not implemented") }

// SessionBase is the ANTHROPIC_BASE_URL for a session whose default provider is provider.
func SessionBase(base, provider string) string { return base + "/p/" + provider }
