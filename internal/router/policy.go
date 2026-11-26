package router

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
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

var (
	kindNames   = [...]string{PolicyNone: "none", PolicySameWorkspace: "same-workspace", PolicyAnyInFence: "any-in-fence"}
	sourceNames = [...]string{PolicyDefault: "default", PolicyEnv: "env", PolicyOverlay: "overlay", PolicyTyped: "typed"}
)

func (k PolicyKind) MarshalText() ([]byte, error) {
	if int(k) >= len(kindNames) || kindNames[k] == "" {
		return nil, nil // the zero kind: Resolved fills it on read
	}
	return []byte(kindNames[k]), nil
}

func (k *PolicyKind) UnmarshalText(b []byte) error {
	*k = 0
	for i, n := range kindNames {
		if n != "" && n == string(b) {
			*k = PolicyKind(i)
		}
	}
	return nil
}

func (s PolicySource) MarshalText() ([]byte, error) {
	return []byte(sourceNames[min(int(s), len(sourceNames)-1)]), nil
}

func (s *PolicySource) UnmarshalText(b []byte) error {
	*s = PolicyDefault
	for i, n := range sourceNames {
		if n == string(b) {
			*s = PolicySource(i)
		}
	}
	return nil
}

// Policy is the typed fallback rule; Agents admits other agents' hops.
type Policy struct {
	Kind   PolicyKind   `json:"kind,omitempty"`
	Agents bool         `json:"agents,omitempty"`
	OffArm bool         `json:"off_arm,omitempty"` // Mode all: own-login hops leaving the arm, only when the fence is open.
	Source PolicySource `json:"source,omitempty"`
}

// Resolved is p, or today's default mode when p is the zero value (a plan or record that lacked a fallback).
func (p Policy) Resolved() Policy {
	if p.Kind != 0 {
		return p
	}
	d, _ := PolicyOf("", PolicyDefault)
	return d
}

// PolicyOf maps a stored mode; empty is none everywhere.
func PolicyOf(mode string, src PolicySource) (Policy, error) {
	switch ParseFallback(mode) {
	case route.FallbackNone:
		return Policy{Kind: PolicyNone, Source: src}, nil
	case route.FallbackPin:
		return Policy{Kind: PolicyAnyInFence, Source: src}, nil
	case route.FallbackAgents:
		return Policy{Kind: PolicyAnyInFence, Agents: true, Source: src}, nil
	case route.FallbackAll:
		return Policy{Kind: PolicyAnyInFence, Agents: true, OffArm: true, Source: src}, nil
	}
	return Policy{}, fmt.Errorf("%s=%q unknown (%s)", EnvFallback, mode, strings.Join(route.FallbackModes, "|"))
}

// Mode is p as the persisted --fallback string (record fallback_policy).
func (p Policy) Mode() string {
	p = p.Resolved()
	switch {
	case p.Kind == PolicyNone:
		return route.FallbackNone
	case p.OffArm:
		return route.FallbackAll
	case p.Agents:
		return route.FallbackAgents
	}
	return route.FallbackPin // AnyInFence without agents, and SameWorkspace.
}

// Typed reports a requested mode: typed or overlay, including an explicit none.
func (p Policy) Typed() bool { return p.Source == PolicyTyped || p.Source == PolicyOverlay }

// FallbackSetting reads the raw mode and source in overlay, process env, vault env order.
func FallbackSetting(ctx context.Context, overlayMode string) (string, PolicySource) {
	raw, src, _ := FallbackFrom(ctx, overlayMode, providers.CustomEnv())
	return raw, src
}

// FallbackFrom is the one resolver: overlay, process env, then env (the vault bucket); origin names the layer.
func FallbackFrom(ctx context.Context, overlayMode string, env map[string]string) (raw string, src PolicySource, origin string) {
	if v := strings.TrimSpace(overlayMode); v != "" {
		return v, PolicyOverlay, "overlay"
	}
	if v := strings.TrimSpace(proc.Value(ctx, EnvFallback)); v != "" {
		return v, PolicyEnv, "process env"
	}
	if v := strings.TrimSpace(env[EnvFallback]); v != "" {
		return v, PolicyEnv, "vault env"
	}
	return "", PolicyDefault, "default"
}

// EffectiveFallback is the parsed mode every reader acts on, with the layer it came from.
func EffectiveFallback(ctx context.Context, overlayMode string, env map[string]string) (mode, origin string) {
	raw, _, origin := FallbackFrom(ctx, overlayMode, env)
	return ParseFallback(raw), origin
}

// ValidFallback reports a typed mode the router accepts.
func ValidFallback(mode string) bool { return slices.Contains(route.FallbackModes, mode) }
