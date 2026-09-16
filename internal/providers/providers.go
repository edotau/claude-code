// Package providers is the model-routing registry: named upstreams, their gateway routes per wire
// dialect, credential sources, and the tier→model pins a launch or routed request resolves against.
package providers

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Wire dialects a provider route can speak.
const (
	DialectAnthropic = "anthropic" // POST {route}/v1/messages
	DialectOpenAI    = "openai"    // POST {route}/chat/completions
	DialectResponses = "responses" // POST {route}/responses
	DialectGemini    = "gemini"    // Google-native generateContent
)

// Auth header styles.
const (
	AuthAPIKey      = "x-api-key"   // Anthropic-native key header
	AuthBearer      = "bearer"      // Authorization: Bearer <cred>
	AuthNone        = "none"        // local servers (ollama, llama.cpp)
	AuthPassthrough = "passthrough" // forward the client's own auth (Claude subscription OAuth)
)

// Provider is one upstream: a direct API or a gateway exposing several dialects under one host.
type Provider struct {
	Name string `json:"-"`
	// Kind is the dialect BaseURL itself speaks; Routes add further dialects (gateway paths).
	Kind    string            `json:"kind"`
	BaseURL string            `json:"base_url"`
	Routes  map[string]string `json:"routes,omitempty"` // dialect → path suffix or absolute URL
	Auth    Auth              `json:"auth"`
	Models  map[string]string `json:"models,omitempty"` // slot (opus/sonnet/haiku/fable) → model id
	Headers map[string]string `json:"headers,omitempty"`
	// OneM opts the provider's Claude ids into the [1m] marker; only true where the upstream serves 1M context.
	OneM bool `json:"one_m,omitempty"`
}

// Auth names where the credential comes from; the first non-empty source wins (see Credential).
type Auth struct {
	Type    string `json:"type"`              // x-api-key | bearer | none | passthrough
	Env     string `json:"env,omitempty"`     // env var, also read from env.d/secrets.env
	File    string `json:"file,omitempty"`    // file holding the credential (~ expanded)
	Command string `json:"command,omitempty"` // shell command printing the credential (keychain, op, OAuth mint)
	TTL     int    `json:"ttl,omitempty"`     // seconds a Command result may be cached; 0 = 300
}

// Route returns the base URL serving dialect: an explicit route (absolute, or a path joined onto
// BaseURL), else BaseURL itself when dialect == Kind.
func (p Provider) Route(dialect string) (string, bool) {
	if r, ok := p.Routes[dialect]; ok && r != "" {
		if u, err := url.Parse(r); err == nil && u.IsAbs() {
			return strings.TrimRight(r, "/"), true
		}
		return strings.TrimRight(p.BaseURL, "/") + "/" + strings.Trim(r, "/"), true
	}
	if dialect == p.Kind && p.BaseURL != "" {
		return strings.TrimRight(p.BaseURL, "/"), true
	}
	return "", false
}

// Dialects lists every dialect the provider can serve, sorted.
func (p Provider) Dialects() []string {
	seen := map[string]bool{}
	if p.Kind != "" && p.BaseURL != "" {
		seen[p.Kind] = true
	}
	for d, r := range p.Routes {
		if r != "" {
			seen[d] = true
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// SlotOf reverse-maps a model id to the slot it occupies in this provider, for cross-provider failover.
func (p Provider) SlotOf(model string) string {
	for slot, id := range p.Models {
		if id == model {
			return slot
		}
	}
	return ""
}

// Registry is the merged provider table plus the default selection and failover ladder.
type Registry struct {
	Default   string               `json:"default"`
	Fallback  []string             `json:"fallback,omitempty"`
	Providers map[string]*Provider `json:"providers"`
}

// Get returns the named provider or an error naming the known ones.
func (r *Registry) Get(name string) (*Provider, error) {
	if p, ok := r.Providers[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("unknown provider %q (known: %s)", name, strings.Join(r.Names(), ", "))
}

// Names lists provider names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.Providers))
	for n := range r.Providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Validate reports registry errors: unknown dialects, missing base URLs, dangling default/fallback names.
func (r *Registry) Validate() error {
	var errs []string
	valid := map[string]bool{DialectAnthropic: true, DialectOpenAI: true, DialectResponses: true, DialectGemini: true}
	for _, n := range r.Names() {
		p := r.Providers[n]
		if !valid[p.Kind] {
			errs = append(errs, fmt.Sprintf("%s: kind %q is not a dialect", n, p.Kind))
		}
		if p.BaseURL == "" && len(p.Routes) == 0 {
			errs = append(errs, fmt.Sprintf("%s: needs base_url or routes", n))
		}
		for d := range p.Routes {
			if !valid[d] {
				errs = append(errs, fmt.Sprintf("%s: route dialect %q unknown", n, d))
			}
		}
		switch p.Auth.Type {
		case AuthAPIKey, AuthBearer, AuthNone, AuthPassthrough:
		default:
			errs = append(errs, fmt.Sprintf("%s: auth.type %q unknown", n, p.Auth.Type))
		}
	}
	for _, n := range append([]string{r.Default}, r.Fallback...) {
		if _, ok := r.Providers[n]; n != "" && !ok {
			errs = append(errs, fmt.Sprintf("default/fallback names unknown provider %q", n))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("providers: %s", strings.Join(errs, "; "))
	}
	return nil
}
