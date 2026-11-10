package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

var gatewayModels = providers.GatewayModels

func modelName(id string) string {
	if canonical, _, ok := models.Resolve(id); ok {
		if entry, ok := models.Lookup(canonical); ok {
			return entry.Name
		}
	}
	words := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' })
	for i, word := range words {
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

// ModelList is the Claude catalog Desktop offers for profile p: the gateway listing, else the profile's pins.
func ModelList(ctx context.Context, reg *providers.Registry, p *providers.Provider) ([]Model, error) {
	infos := catalogInfos(ctx, p)
	var out []Model
	seen := map[string]bool{}
	for _, info := range infos {
		id := models.Strip1M(info.ID)
		if models.FamilyKind(id) != models.FamilyClaude || seen[id] || !resolves(reg, p, id) {
			continue
		}
		seen[id] = true
		name := info.Name
		if name == "" {
			name = id
		}
		out = append(out, Model{Name: id, LabelOverride: name})
	}
	slices.SortFunc(out, func(a, b Model) int { return strings.Compare(a.Name, b.Name) })
	if len(out) == 0 {
		return nil, fmt.Errorf("profile %s has no Claude models in its catalog", p.Name)
	}
	return out, nil
}

// catalogInfos lists the gateway catalog, falling back to the profile's model pins.
func catalogInfos(ctx context.Context, p *providers.Provider) []providers.ModelInfo {
	var infos []providers.ModelInfo
	if p.IsAIGateway() {
		var err error
		infos, err = gatewayModels(ctx, p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "configure desktop: gateway catalog unavailable; using profile model pins")
		}
	}
	if len(infos) == 0 {
		for _, id := range p.Models {
			id = models.Strip1M(id)
			infos = append(infos, providers.ModelInfo{ID: id, Name: modelName(id)})
		}
	}
	return infos
}

// resolves mirrors the proxy: the id must route to profile p on /p/<profile>.
func resolves(reg *providers.Registry, p *providers.Provider, id string) bool {
	t := providers.ParseTarget(reg, id, p)
	return t.Provider.Name == p.Name && t.Model != ""
}

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
	opts.Headers = gateway.HarnessHeaders()
	opts.Headers[gateway.AgentHeader] = "desktop"
	base := gateway.Base(gateway.DefaultPort)
	if dryRun {
		if st, ok := gateway.Running(ctx); ok {
			base = gateway.Base(st.Port)
		}
	} else if base, err = ensure(ctx); err != nil {
		return opts, errors.New("proxy unavailable; run gateway proxy --start")
	}
	opts.BaseURL = gateway.SessionBase(base, p.Name)
	return opts, nil
}
