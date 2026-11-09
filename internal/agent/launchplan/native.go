package launchplan

import (
	"context"
	"fmt"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentcfg"
	"github.com/edotau/claude-code/internal/providers"
)

// Native is the argv, env and unset names an agent's own CLI is launched with.
type Native struct {
	Args  []string
	Env   map[string]string
	Unset []string
}

// Map turns c into the native launch the agent's interactive path builds for c.Profile.
// Only copilot is a dry preview; codex, gemini and opencode run Schema.Configure with its launch side effects.
func Map(c agentcfg.Config) (Native, error) {
	switch c.Agent {
	case "codex":
		return mapCodex(c)
	case "opencode":
		return mapOpencode(c)
	case "copilot":
		return mapCopilot(c)
	case "gemini":
		return mapGemini(c)
	case "claude":
		return mapClaude(c)
	}
	return Native{}, fmt.Errorf("launchplan: no mapper for agent %q", c.Agent)
}

// resolved is a roster agent with the registry target its profile selects.
type resolved struct {
	spec   agent.Schema
	reg    *providers.Registry
	target providers.Target
}

// resolve runs the launch path's selection: profile, then the agent's target, then its model pin.
func resolve(c agentcfg.Config) (resolved, error) {
	spec, ok := agent.LookupSchema(c.Agent)
	if !ok || !spec.External() {
		return resolved{}, fmt.Errorf("launchplan: agent %q is not registered", c.Agent)
	}
	reg, err := providers.Load()
	if err != nil {
		return resolved{}, err
	}
	sel, err := providers.Select(reg, c.Profile)
	if err != nil {
		return resolved{}, err
	}
	t, _, err := spec.ResolveTarget(reg, sel, c.Profile != "")
	if err != nil {
		return resolved{}, err
	}
	applied := t.Provider == nil || t.Provider == sel.Aliases["opus"].Provider
	t, _ = agent.ResolveAgentModel(agent.ModelAsk{Agent: c.Agent, Flag: c.Model, Target: t, Applied: applied && c.Model == ""})
	return resolved{spec, reg, t}, nil
}

// viaConfigure is the launch built by Schema.Configure (no dry flag: may start the router, mint a token, write config).
func viaConfigure(c agentcfg.Config) (Native, error) {
	r, err := resolve(c)
	if err != nil {
		return Native{}, err
	}
	env, prefix, err := r.spec.Configure(context.Background(), r.target, "")
	if err != nil {
		return ownLoginOr(r.spec, err)
	}
	return Native{Args: prefix, Env: envMap(env)}, nil
}

// ownLoginOr is the plain launch for agent.ErrOwnLogin, else err.
func ownLoginOr(spec agent.Schema, err error) (Native, error) {
	if err != agent.ErrOwnLogin {
		return Native{}, err
	}
	env, prefix := spec.LaunchSpec()
	return Native{Args: prefix, Env: envMap(env)}, nil
}

// mapClaude is the session selection only (profile and main model); the ANTHROPIC_* routing env stays in claude.Claude.
func mapClaude(c agentcfg.Config) (Native, error) {
	env := map[string]string{}
	if c.Profile != "" {
		env[providers.EnvSessionProfile] = c.Profile
	}
	if c.Model != "" {
		env["ANTHROPIC_MODEL"] = c.Model
	}
	return Native{Env: env}, nil
}

// mapCodex is codex's launch: Schema.Configure over the profile's target.
func mapCodex(c agentcfg.Config) (Native, error) { return viaConfigure(c) }

// mapCopilot is copilot's launch: its native registry (BYOK env and --model), dry so nothing is minted or written.
func mapCopilot(c agentcfg.Config) (Native, error) {
	r, err := resolve(c)
	if err != nil {
		return Native{}, err
	}
	ctx := context.Background()
	if !r.spec.Eng().NativeRegistry(ctx, r.target) {
		return viaConfigure(c)
	}
	cr, err := r.spec.Eng().RegistryLaunch(ctx, r.reg, r.target, nil, true)
	if err != nil {
		return Native{}, err
	}
	lenv, lprefix := r.spec.LaunchSpec()
	args := append(append(append([]string{}, lprefix...), cr.Argv...), cr.Args...)
	return Native{Args: args, Env: envMap(append(lenv, cr.Env...)), Unset: cr.Unset}, nil
}

// mapGemini is gemini's launch: Schema.Configure over the profile's target.
func mapGemini(c agentcfg.Config) (Native, error) { return viaConfigure(c) }

// mapOpencode is opencode's launch: Schema.Configure over the profile's target.
func mapOpencode(c agentcfg.Config) (Native, error) { return viaConfigure(c) }
