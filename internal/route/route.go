// Package route is the one route type every dispatch hop shares; it imports only the standard library.
package route

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode"
)

// Providers lists the provider names ParseLegacy knows; agentconfig's test keeps it equal to the registry's.
var Providers = []string{"anthropic", "databricks", "github", "google", "openai"}

// Route is the unit that is assigned whole: where a run goes and as whom; the json keys are the overlay's.
type Route struct {
	Provider string `json:"provider,omitempty"`
	Profile  string `json:"profile,omitempty"`
	Identity string `json:"identity,omitempty"`
	Account  string `json:"account,omitempty"`
}

// Target is a route plus the plain model id to ask it for.
type Target struct {
	Route
	Model string `json:"model,omitempty"`
}

// IsZero reports a route with no key set.
func (r Route) IsZero() bool { return r == Route{} }

// Pick returns the first non-zero layer whole; keys never mix across layers.
func Pick(layers ...Route) Route {
	if i := slices.IndexFunc(layers, func(r Route) bool { return !r.IsZero() }); i >= 0 {
		return layers[i]
	}
	return Route{}
}

// ParseLegacy reads "provider:profile", "provider" or a bare profile; anything malformed is the zero route.
func ParseLegacy(s string) Route {
	s = strings.TrimSpace(s)
	provider, profile, named := strings.Cut(s, ":")
	switch {
	case s == "":
	case !named && !slices.Contains(Providers, s):
		return Route{Profile: s}
	case slices.Contains(Providers, provider) && (!named || (profile != "" && !strings.Contains(profile, ":"))):
		return Route{Provider: provider, Profile: profile}
	}
	return Route{}
}

// Validate applies the route's own rules; an error names the key, never its value.
func (r Route) Validate() error {
	switch {
	case r.Provider == "" && !r.IsZero():
		return errors.New("route: profile, identity and account need a provider")
	case r.Identity != "" && r.Provider != "databricks":
		return errors.New("identity: databricks only")
	case r.Account != "" && r.Provider != "github":
		return errors.New("account: github only")
	case strings.ContainsFunc(r.Profile, func(c rune) bool { return c == ':' || unicode.IsSpace(c) }):
		return errors.New("profile: want a name without ':' or spaces")
	}
	return nil
}

// JSON is the canonical one-line object (keys: provider, profile, identity, account, model) for --overlay.
func (t Target) JSON() string {
	var b strings.Builder
	b.WriteByte('{')
	for _, kv := range [][2]string{{"provider", t.Provider}, {"profile", t.Profile}, {"identity", t.Identity},
		{"account", t.Account}, {"model", t.Model}} {
		if kv[1] == "" {
			continue
		}
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv[0])
		v, _ := json.Marshal(kv[1])
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.String()
}
