package router

import (
	"context"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

// PolicyKind is how far an unavailable hop may move.
type PolicyKind uint8

const (
	PolicyNone          PolicyKind = iota + 1 // The planned workspace only; runners still walk.
	PolicySameWorkspace                       // Every runner on the first pool workspace, no other workspace.
	PolicyAnyInFence                          // Every runner on every pool workspace the fence allows.
)

// PolicySource is where the mode came from; only typed and overlay modes are requests.
type PolicySource uint8

const (
	PolicyDefault PolicySource = iota // Nothing set anywhere.
	PolicyEnv                         // AGENT_ERROR_FALLBACK, process or vault.
	PolicyOverlay                     // Overlay or COWORKER_OVERLAY fallback.
	PolicyTyped                       // --fallback, or a replayed typed record.
)

// Policy is the typed fallback rule; Agents admits other agents' hops.
type Policy struct {
	Kind   PolicyKind
	Agents bool
	OffArm bool // Mode all: own-login hops leaving the arm, only when the fence is open.
	Source PolicySource
}

// PolicyOf maps a stored mode; empty is none everywhere.
func PolicyOf(mode string, src PolicySource) (Policy, error) {
	switch ParseFallback(mode) {
	case FallbackNone:
		return Policy{Kind: PolicyNone, Source: src}, nil
	case FallbackPin:
		return Policy{Kind: PolicyAnyInFence, Source: src}, nil
	case FallbackAgents:
		return Policy{Kind: PolicyAnyInFence, Agents: true, Source: src}, nil
	case FallbackAll:
		return Policy{Kind: PolicyAnyInFence, Agents: true, OffArm: true, Source: src}, nil
	}
	return Policy{}, fmt.Errorf("%s=%q unknown (%s)", EnvFallback, mode, strings.Join(FallbackModes, "|"))
}

// Mode is p as the persisted --fallback string (record fallback_policy).
func (p Policy) Mode() string {
	switch {
	case p.Kind == PolicyNone:
		return FallbackNone
	case p.OffArm:
		return FallbackAll
	case p.Agents:
		return FallbackAgents
	}
	return FallbackPin // AnyInFence without agents, and SameWorkspace.
}

// Typed reports a requested mode: typed or overlay, including an explicit none.
func (p Policy) Typed() bool { return p.Source == PolicyTyped || p.Source == PolicyOverlay }

// FallbackSetting reads the raw mode and source in overlay, process env, vault env order.
func FallbackSetting(ctx context.Context, overlayMode string) (string, PolicySource) {
	if v := strings.TrimSpace(overlayMode); v != "" {
		return v, PolicyOverlay
	}
	if v := strings.TrimSpace(proc.Value(ctx, EnvFallback)); v != "" {
		return v, PolicyEnv
	}
	if v := strings.TrimSpace(providers.CustomEnv()[EnvFallback]); v != "" {
		return v, PolicyEnv
	}
	return "", PolicyDefault
}
