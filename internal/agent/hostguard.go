package agent

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/envscrub"
	"github.com/edotau/claude-code/internal/providers"
)

type (
	ownProviderKey struct{}
	isolateKey     struct{}
)

// withIsolation makes every hop under ctx drop other profiles' credential env (batch tasks run concurrently).
func withIsolation(ctx context.Context) context.Context {
	return context.WithValue(ctx, isolateKey{}, true)
}

// withOwnProvider marks p as the one profile this hop may hold credentials for, when ctx asks for isolation.
func withOwnProvider(ctx context.Context, p *providers.Provider) context.Context {
	if ctx.Value(isolateKey{}) == nil {
		return ctx
	}
	return context.WithValue(ctx, ownProviderKey{}, p)
}

func ownProviderFrom(ctx context.Context) *providers.Provider {
	p, _ := ctx.Value(ownProviderKey{}).(*providers.Provider)
	return p
}

// guardRouteHost fails a hop before any child starts when p's credential was issued for another host than its route.
func guardRouteHost(ctx context.Context, p *providers.Provider) error {
	if p.Preset == "" {
		return nil
	}
	cred, err := providers.Credential(ctx, p)
	if errors.Is(err, providers.ErrPassthrough) || (err == nil && cred == "") {
		return nil // no minted credential to misroute
	}
	if err != nil {
		return err // an unresolvable credential must not skip the host check
	}
	return providers.CheckRouteHost(p, cred)
}

// guardProvider is the provider whose credential the hop sends; nil for an unpinned vendor leg (own login).
func guardProvider(r Runner, req Request) (*providers.Provider, error) {
	if l, ok := r.(hubLeg); ok {
		return l.provider, nil
	}
	if req.Provider == "" && r.Name() != "claude" {
		return nil, nil
	}
	_, t, err := resolveTarget(req)
	if err != nil {
		return nil, err
	}
	return t.Provider, nil
}

// checkIsolation fails an isolated hop up front when the registry cannot load, since the scrub would then be skipped.
func checkIsolation(ctx context.Context) error {
	if ctx.Value(isolateKey{}) == nil {
		return nil
	}
	_, err := providers.Load()
	return err
}

// scrubForeignCredentials drops every other profile's credential env name from env, so a hop holds only its own.
func scrubForeignCredentials(env []string, own *providers.Provider, keepEnv []string) []string {
	reg, err := providers.Load()
	if err != nil || own == nil {
		return env
	}
	mine := append(providers.CredentialNames(own), keepEnv...)
	foreign := map[string]bool{}
	for _, p := range reg.Providers {
		if p == nil || p.Name == own.Name {
			continue
		}
		for _, n := range providers.CredentialNames(p) {
			if !slices.Contains(mine, n) {
				foreign[n] = true
			}
		}
	}
	keep := envscrub.KeepSet(keepEnv)
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return foreign[name] && !keep[name]
	})
}
