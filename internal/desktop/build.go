package desktop

import (
	"context"
	"fmt"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

// BuildOptions fills the non-revert Options for profile (default: session profile); helper and ensure are test seams.
func BuildOptions(ctx context.Context, reg *providers.Registry, profile string, dryRun bool, opts Options,
	helper func(string) (string, []string, bool, error), ensure func(context.Context) (string, error)) (Options, error) {
	sel, err := providers.Select(reg, profile)
	if err != nil {
		return opts, err
	}
	p := sel.Provider
	if p.Auth.Method() == providers.AuthCLI {
		return opts, fmt.Errorf("profile %s uses a CLI login; select an API profile", p.Name)
	}
	opts.Profile = p.Name
	opts.HelperPath, opts.HelperArgs, opts.HelperWindows, err = helper(p.Name)
	if err != nil {
		return opts, err
	}
	opts.Models, err = ModelList(ctx, reg, p)
	if err != nil {
		return opts, err
	}
	opts.Headers = proxy.HarnessHeaders()
	opts.Headers[proxy.AgentHeader] = "desktop"
	base := proxy.Base(proxy.DefaultPort)
	if dryRun {
		if st, ok := proxy.Running(ctx); ok {
			base = proxy.Base(st.Port)
		}
	} else if base, err = ensure(ctx); err != nil {
		return opts, fmt.Errorf("proxy unavailable; run gateway proxy --start")
	}
	opts.BaseURL = proxy.SessionBase(base, p.Name)
	return opts, nil
}
