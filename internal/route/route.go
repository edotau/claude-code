// Package route is the one route type every dispatch hop shares; it imports only the standard library.
package route

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"unicode"
)

// Providers lists the provider names ParseLegacy knows; agentconfig's test keeps it equal to the registry's.
var Providers = []string{"anthropic", "databricks", "github", "google", "magnitude", "openai"}

// AutoArm is the databricks profile an ask resolves to one workspace; unresolved, it confines nothing yet.
const AutoArm = "auto"

// Route is the unit that is assigned whole: where a run goes and as whom; the json keys are the overlay's.
type Route struct {
	Endpoint string `json:"endpoint,omitempty"`
	Provider string `json:"provider,omitempty"`
	Profile  string `json:"profile,omitempty"`
	Identity string `json:"identity,omitempty"`
	Account  string `json:"account,omitempty"`
	Host     string `json:"host,omitempty"`
	Remote   string `json:"remote,omitempty"`
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

// ParseLegacy reads an old record's "provider:profile", else FromWord's forms; anything malformed is the zero route.
func ParseLegacy(s string) Route {
	s = strings.TrimSpace(s)
	provider, profile, named := strings.Cut(s, ":")
	switch {
	case !named:
		return FromWord(s)
	case slices.Contains(Providers, provider) && profile != "" && !strings.Contains(profile, ":"):
		return Route{Provider: provider, Profile: profile}
	}
	return Route{}
}

// FromWord reads one typed word: a provider name, else a profile; the colon form is refused before it.
func FromWord(s string) Route {
	switch s = strings.TrimSpace(s); {
	case s == "":
		return Route{}
	case slices.Contains(Providers, s):
		return Route{Provider: s}
	}
	return Route{Profile: s}
}

// Label is r as users read it: provider·profile, or whichever of the two is set.
func (r Route) Label() string {
	if r.Provider == "" || r.Profile == "" {
		return r.Provider + r.Profile
	}
	return r.Provider + "·" + r.Profile
}

// ColonForm reports a provider:profile value (an overlay preset); JSON, "-" and an existing file are not one.
func ColonForm(v string) bool {
	v = strings.TrimSpace(v)
	if _, err := os.Lstat(v); err == nil {
		return false
	}
	provider, _, named := strings.Cut(v, ":")
	return named && !strings.HasPrefix(v, "{") && slices.Contains(Providers, provider)
}

// named reports a key that only means something under a provider.
func (r Route) named() bool {
	return r.Profile != "" || r.Identity != "" || r.Account != "" || r.Endpoint != ""
}

// Validate applies the route's own rules; an error names the key, never its value.
func (r Route) Validate() error {
	switch {
	case r.Provider == "" && r.named():
		return errors.New("route: endpoint, profile, identity and account need a provider")
	case r.Endpoint != "" && r.Profile == "":
		return errors.New("route: endpoint needs a profile")
	case r.Host != "" && r.Remote != "":
		return errors.New("route: host and remote are exclusive")
	case strings.ContainsFunc(r.Profile, func(c rune) bool { return c == ':' || unicode.IsSpace(c) }):
		return errors.New("profile: want a name without ':' or spaces")
	}
	return errors.Join(r.checkEndpoint(), r.checkScoped())
}

// checkEndpoint takes a short name (proxy, direct, a route id) or an https URL (http on loopback) with no userinfo.
func (r Route) checkEndpoint() error {
	e := r.Endpoint
	if e == "" {
		return nil
	}
	if !strings.Contains(e, "://") {
		if strings.ContainsFunc(e, func(c rune) bool {
			return c > unicode.MaxASCII || !unicode.IsLower(c) && !unicode.IsDigit(c) && c != '-' && c != '_'
		}) {
			return errors.New("endpoint: want proxy, direct, a route name or an https URL")
		}
		return nil
	}
	u, err := url.Parse(e)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("endpoint: want a URL with a host and no credentials, query or fragment")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
		return errors.New("endpoint: a URL must be https (http only on loopback)")
	}
	return nil
}

// ReadDoc reads the unit and fence of one flat JSON overlay document; no fence key reads as Open, a bad unit errors.
func ReadDoc(raw string) (Route, Fence, error) {
	var doc struct {
		Route
		Fence *Fence `json:"fence"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &doc); err != nil {
		return Route{}, Fence{}, errors.New("not valid JSON")
	}
	if doc.Provider != "" && !slices.Contains(Providers, doc.Provider) {
		return Route{}, Fence{}, fmt.Errorf("unknown provider (%s)", strings.Join(Providers, "|"))
	}
	if err := doc.Validate(); err != nil {
		return Route{}, Fence{}, err
	}
	if doc.Fence == nil {
		return doc.Route, Fence{Open: true}, nil
	}
	return doc.Route, *doc.Fence, nil
}

// checkScoped refuses identity off Databricks and account off GitHub.
func (r Route) checkScoped() error {
	switch {
	case r.Identity != "" && r.Provider != "databricks":
		return errors.New("identity: databricks only")
	case r.Account != "" && r.Provider != "github":
		return errors.New("account: github only")
	}
	return nil
}

// JSON is the canonical one-line overlay object: the unit keys, then model.
func (t Target) JSON() string {
	var b strings.Builder
	b.WriteByte('{')
	for _, kv := range [][2]string{{"endpoint", t.Endpoint}, {"provider", t.Provider}, {"profile", t.Profile}, {"identity", t.Identity},
		{"account", t.Account}, {"host", t.Host}, {"remote", t.Remote}, {"model", t.Model}} {
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
