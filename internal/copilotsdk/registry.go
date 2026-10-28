package copilotsdk

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	copilot "github.com/github/copilot-sdk/go"
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
		if (stripPrefix != "" && strings.HasPrefix(name, stripPrefix)) || inList(unset, name) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, set...)
}

func inList(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// validateRegistryOptions fails closed before any runtime starts: no singular BYOK, a token source, a registered pick.
func validateRegistryOptions(o Options) error {
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

func (o Options) multiProvider() bool { return len(o.Providers) > 0 || len(o.Models) > 0 }

// namedProviders copies the credential-free providers and routes every one's auth through the token callback.
func namedProviders(o Options) ([]copilot.NamedProviderConfig, []copilot.ProviderModelConfig) {
	cb := func(copilot.ProviderTokenArgs) (string, error) { return o.Token() }
	ps := make([]copilot.NamedProviderConfig, len(o.Providers))
	for i, p := range o.Providers {
		p.APIKey, p.BearerToken, p.BearerTokenProvider = "", "", cb
		ps[i] = p
	}
	return ps, append([]copilot.ProviderModelConfig(nil), o.Models...)
}

// probeMultiProvider gates the route on the running CLI's version: older runtimes ignore named providers.
func probeMultiProvider(ctx context.Context, client *copilot.Client) error {
	st, err := client.GetStatus(ctx)
	if err != nil {
		return fmt.Errorf("%w: cannot read CLI version: %v", ErrNoMultiProvider, err)
	}
	if !versionAtLeast(st.Version, MinMultiProviderCLI) {
		return fmt.Errorf("%w: CLI %q is older than %s (run `copilot update`)", ErrNoMultiProvider, st.Version, MinMultiProviderCLI)
	}
	return nil
}

// versionAtLeast compares dotted numeric versions; an unparsable v is never enough.
func versionAtLeast(v, min string) bool {
	a, b := versionParts(v), versionParts(min)
	if a == nil {
		return false
	}
	for i := range b {
		var x int
		if i < len(a) {
			x = a[i]
		}
		if x != b[i] {
			return x > b[i]
		}
	}
	return true
}

func versionParts(v string) []int {
	var out []int
	for _, s := range strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".") {
		n, err := strconv.Atoi(strings.SplitN(s, "-", 2)[0])
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// selectRegistered pins the session to o.Model; an unknown id errors, so a pick never falls through to GitHub.
func selectRegistered(ctx context.Context, session *copilot.Session, o Options) error {
	var opts *copilot.SetModelOptions
	if o.ReasoningEffort != "" {
		opts = &copilot.SetModelOptions{ReasoningEffort: &o.ReasoningEffort}
	}
	if err := session.SetModel(ctx, o.Model, opts); err != nil {
		return fmt.Errorf("%w: select %s: %v", ErrUnregisteredPick, o.Model, err)
	}
	return nil
}
