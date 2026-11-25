package providers

import (
	"errors"
	"fmt"
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

// CheckLastResort reports why p could not carry options.last_resort, or nil.
func CheckLastResort(name string, p Provider) error {
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
	if !strings.HasPrefix(p.Routes["anthropic"], "http") {
		errs = append(errs, n+": options.last_resort needs routes.anthropic as an http(s) URL")
	}
	if p.Concurrency != 1 {
		errs = append(errs, fmt.Sprintf("%s: options.last_resort needs concurrency 1, got %d", n, p.Concurrency))
	}
	return errs
}
