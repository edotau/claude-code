package copilot

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/edotau/claude-code/internal/textutil"
)

// MinMultiProviderCLI is the oldest copilot CLI proven to serve SDK named providers (probed 2026-10-28).
const MinMultiProviderCLI = "1.0.104"

// Named failures of the multi-provider route; callers match with errors.Is.
var (
	ErrNoMultiProvider  = errors.New("copilot CLI lacks named-provider support")
	ErrUnregisteredPick = errors.New("model is not in the session registry")
)

// registryEnv is base minus the exact unset names and any stripPrefix name, plus set.
func registryEnv(base, set, unset []string, stripPrefix string) []string {
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if (stripPrefix != "" && strings.HasPrefix(name, stripPrefix)) || slices.Contains(unset, name) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, set...)
}

// validateRegistryOptions fails closed before any runtime starts: no singular BYOK, a token source, a registered pick.
func validateRegistryOptions(o SDKOptions) error {
	switch {
	case o.Token == nil:
		return errors.New("copilot registry route needs a session token source")
	case len(o.ChildEnv) == 0:
		return errors.New("copilot registry route needs the private home env (ChildEnv)")
	case o.GitHubToken != "" || o.UseLoggedInUser:
		return errors.New("copilot registry route must not carry a GitHub login")
	}
	for _, kv := range o.Env {
		if strings.HasPrefix(kv, "COPILOT_PROVIDER_") || strings.HasPrefix(kv, "COPILOT_PROVIDERS_CONFIG=") {
			return errors.New("copilot registry route cannot mix with a singular provider env block")
		}
	}
	for _, m := range o.Models {
		if m.Provider+"/"+m.ID == o.Model {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrUnregisteredPick, o.Model)
}

func (o SDKOptions) multiProvider() bool { return len(o.Providers) > 0 || len(o.Models) > 0 }

// NamedProvider is one credential-free upstream of the multi-provider route.
type NamedProvider struct {
	Name, Type, WireAPI, BaseURL string
	Headers                      map[string]string
}

// RegistryModel is one model served by a NamedProvider.
type RegistryModel struct {
	ID, Provider, WireModel, ModelID, Name                   string
	MaxPromptTokens, MaxContextWindowTokens, MaxOutputTokens int
}

// namedProviders builds the SDK providers with credentials cleared; every one's auth goes through the token callback.
func namedProviders(o SDKOptions) ([]copilot.NamedProviderConfig, []copilot.ProviderModelConfig) {
	cb := func(copilot.ProviderTokenArgs) (string, error) { return o.Token() }
	ps := make([]copilot.NamedProviderConfig, len(o.Providers))
	for i, p := range o.Providers {
		ps[i] = copilot.NamedProviderConfig{Name: p.Name, Type: p.Type, WireAPI: p.WireAPI, BaseURL: p.BaseURL,
			Headers: p.Headers, APIKey: "", BearerToken: "", BearerTokenProvider: cb}
	}
	ms := make([]copilot.ProviderModelConfig, len(o.Models))
	for i, m := range o.Models {
		ms[i] = copilot.ProviderModelConfig{ID: m.ID, Provider: m.Provider, WireModel: m.WireModel, ModelID: m.ModelID,
			Name: m.Name, MaxPromptTokens: m.MaxPromptTokens, MaxContextWindowTokens: m.MaxContextWindowTokens,
			MaxOutputTokens: m.MaxOutputTokens}
	}
	return ps, ms
}

// probeMultiProvider gates the route on the running CLI's version: older runtimes ignore named providers.
func probeMultiProvider(ctx context.Context, client *copilot.Client) error {
	st, err := client.GetStatus(ctx)
	if err != nil {
		return fmt.Errorf("%w: cannot read CLI version: %v", ErrNoMultiProvider, err)
	}
	if order, ok := textutil.CompareVersions(st.Version, MinMultiProviderCLI); !ok || order < 0 {
		return fmt.Errorf("%w: CLI %q is older than %s (run `copilot update`)",
			ErrNoMultiProvider, st.Version, MinMultiProviderCLI)
	}
	return nil
}

// selectRegistered pins the session to o.Model; an unknown id errors, so a pick never falls through to GitHub.
func selectRegistered(ctx context.Context, session *copilot.Session, o SDKOptions) error {
	var opts *copilot.SetModelOptions
	if o.ReasoningEffort != "" {
		opts = &copilot.SetModelOptions{ReasoningEffort: &o.ReasoningEffort}
	}
	if err := session.SetModel(ctx, o.Model, opts); err != nil {
		return fmt.Errorf("%w: select %s: %v", ErrUnregisteredPick, o.Model, err)
	}
	return nil
}

// ClientEnv is the child process env the SDK client would start the CLI with.
func ClientEnv(o SDKOptions) ([]string, error) {
	co, err := BuildClientOptions(o)
	if err != nil {
		return nil, err
	}
	return co.Connection.(copilot.StdioConnection).Env, nil
}
