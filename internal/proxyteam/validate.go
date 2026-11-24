package proxyteam

import (
	"fmt"
	"slices"
)

type adder func(team, field, format string, a ...any)

// Validate lists every problem of c, one per broken rule; an empty result means every lookup is safe.
func Validate(c Config) []Problem {
	var ps []Problem
	add := func(team, field, format string, a ...any) {
		ps = append(ps, Problem{team, field, fmt.Sprintf(format, a...)})
	}
	if c.Schema != schemaVersion {
		add("", "schema", "version %d, want %d", c.Schema, schemaVersion)
	}
	validateNamesAndKinds(c, add)
	var defaults, shared int
	for _, t := range c.Teams {
		validateValues(t, add)
		validateChain(c, t, add)
		defaults += b2i(t.Default)
		shared += b2i(t.Lifetime == LifetimeShared)
	}
	if defaults != 1 {
		add("", "default", "want exactly one default team, have %d", defaults)
	}
	if shared != 1 {
		add("", "lifetime", "want exactly one team with lifetime %q, have %d", LifetimeShared, shared)
	}
	return ps
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func validateNamesAndKinds(c Config, add adder) {
	names, claimed := map[string]bool{}, map[Kind]string{}
	for _, t := range c.Teams {
		switch {
		case t.Name == "":
			add("", "name", "a team has an empty name")
		case names[t.Name]:
			add(t.Name, "name", "the name is used more than once")
		}
		names[t.Name] = true
		if len(t.Kinds) == 0 && !t.Default {
			add(t.Name, "kinds", "empty on a team that is not the default")
		}
		validateClaims(t, claimed, add)
	}
}

func validateClaims(t Team, claimed map[Kind]string, add adder) {
	for _, k := range t.Kinds {
		if !slices.Contains(Kinds(), k) {
			add(t.Name, "kinds", "unknown kind %q (one of %v)", k, Kinds())
		} else if prev, dup := claimed[k]; dup && prev == t.Name {
			add(t.Name, "kinds", "kind %q is listed more than once", k)
		} else if dup {
			add(t.Name, "kinds", "kind %q is already claimed by %q", k, prev)
		} else {
			claimed[k] = t.Name
		}
	}
}

func validateValues(t Team, add adder) {
	hostOK := slices.Contains(Hosts(), t.Host)
	lifeOK := slices.Contains(Lifetimes(), t.Lifetime)
	if !hostOK {
		add(t.Name, "host", "unknown host %q (one of %v)", t.Host, Hosts())
	}
	if !lifeOK {
		add(t.Name, "lifetime", "unknown lifetime %q (one of %v)", t.Lifetime, Lifetimes())
	}
	if !slices.Contains(Borrows(), t.Borrow) {
		add(t.Name, "borrow", "unknown borrow %q (one of %v)", t.Borrow, Borrows())
	}
	if hostOK && lifeOK && pairs[t.Host] != t.Lifetime {
		add(t.Name, "lifetime", "host %q runs with lifetime %q, not %q", t.Host, pairs[t.Host], t.Lifetime)
	}
	if t.Reserved < 0 {
		add(t.Name, "reserved", "negative: %d", t.Reserved)
	} else if t.Reserved == 0 && t.Lifetime == LifetimeShared {
		add(t.Name, "reserved", "0 on the shared team")
	}
}

func validateChain(c Config, t Team, add adder) {
	if t.Lifetime == LifetimeShared {
		if len(t.Fallback) > 0 {
			add(t.Name, "fallback", "the shared team has no fallback")
		}
		if !slices.Contains(t.Kinds, KindInteractive) {
			add(t.Name, "kinds", "the shared team must claim %q", KindInteractive)
		}
		return
	}
	if len(t.Fallback) == 0 {
		add(t.Name, "fallback", "empty on a team that is not the shared team")
		return
	}
	validateFallbackNames(c, t, add)
	last := t.Fallback[len(t.Fallback)-1]
	if i := c.index(last); i >= 0 && c.Teams[i].Lifetime != LifetimeShared {
		add(t.Name, "fallback", "the chain ends at %q, not the shared team", last)
	}
	if cyclic(c, t.Name) {
		add(t.Name, "fallback", "the chain loops back to the team")
	}
}

func validateFallbackNames(c Config, t Team, add adder) {
	for _, n := range t.Fallback {
		if n == t.Name {
			add(t.Name, "fallback", "names itself")
		} else if i := c.index(n); i < 0 {
			add(t.Name, "fallback", "unknown team %q", n)
		} else if c.Teams[i].Borrow == BorrowNever {
			add(t.Name, "fallback", "team %q never lends", n)
		}
	}
}

// cyclic reports whether following fallback names from start returns to start through another team.
func cyclic(c Config, start string) bool {
	seen, stack := map[string]bool{}, []string{start}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		i := c.index(cur)
		if i < 0 {
			continue
		}
		for _, n := range c.Teams[i].Fallback {
			if n == start && cur != start {
				return true
			}
			if !seen[n] {
				seen[n] = true
				stack = append(stack, n)
			}
		}
	}
	return false
}
