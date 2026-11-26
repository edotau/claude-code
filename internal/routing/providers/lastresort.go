package providers

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// LastResort returns the first row by name order with options.last_resort, or nil.
func LastResort(reg *Registry) *Provider {
	if reg == nil {
		return nil
	}
	for _, n := range reg.Names() {
		if p := reg.Providers[n]; p != nil && p.Options.LastResort {
			return p
		}
	}
	return nil
}

func lastResortLines(p *Provider) []string {
	if !p.Options.LastResort {
		return nil
	}
	return []string{"options.last_resort  true"}
}

// CheckLastResort reports why p could not carry options.last_resort, or nil; another holder in reg refuses it.
func CheckLastResort(reg *Registry, name string, p Provider) error {
	for _, n := range reg.Names() {
		if o := reg.Providers[n]; n != name && o != nil && o.Options.LastResort {
			return fmt.Errorf("%s already holds options.last_resort; set it false there first", n)
		}
	}
	p.Options.LastResort = true
	if errs := validateLastResort(name, &p); len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// validateLastResort needs a last-resort row to be a one-at-a-time, non-preset row with an anthropic route.
func validateLastResort(n string, p *Provider) []string {
	if !p.Options.LastResort {
		return nil
	}
	var errs []string
	if p.Preset != "" {
		errs = append(errs, n+": options.last_resort applies to non-preset rows only")
	}
	if err := loopbackURL(p.Routes["anthropic"]); err != nil {
		errs = append(errs, n+": options.last_resort needs routes.anthropic as a loopback http(s) URL: "+err.Error())
	}
	if p.BaseURL != "" {
		if err := loopbackURL(p.BaseURL); err != nil {
			errs = append(errs, n+": options.last_resort base_url must be a loopback http(s) URL: "+err.Error())
		}
	}
	if p.Concurrency != 1 {
		errs = append(errs, fmt.Sprintf("%s: options.last_resort needs concurrency 1, got %d", n, p.Concurrency))
	}
	return errs
}

// loopbackURL accepts only http(s) to 127.0.0.1, localhost or ::1 with no userinfo, query or fragment.
func loopbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return fmt.Errorf("%q is not a URL", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not http or https", u.Scheme)
	}
	if !slices.Contains([]string{"127.0.0.1", "localhost", "::1"}, u.Hostname()) {
		return fmt.Errorf("host %q is not 127.0.0.1, localhost or ::1", u.Hostname())
	}
	return bareURL(u, raw)
}

func bareURL(u *url.URL, raw string) error {
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return errors.New("userinfo, query and fragment are not allowed")
	}
	return nil
}

// validateOneLastResort errors when more than one row carries options.last_resort, naming the first two.
func validateOneLastResort(r *Registry) []string {
	var held []string
	for _, n := range r.Names() {
		if p := r.Providers[n]; p != nil && p.Options.LastResort {
			held = append(held, n)
		}
	}
	if len(held) < 2 {
		return nil
	}
	return []string{fmt.Sprintf("options.last_resort is set on %s and %s; only one row may hold it", held[0], held[1])}
}
