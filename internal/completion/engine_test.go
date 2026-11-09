package completion

import (
	"slices"
	"testing"
)

func syntheticEngine(tree map[string]*Node) Engine {
	return Engine{
		Tree: func() map[string]*Node { return tree },
		Resolve: func(word string) (string, bool) {
			if word == "alias" {
				word = "verb"
			}
			_, ok := tree[word]
			return word, ok
		},
		Verbs: func() []Verb {
			return []Verb{{Name: "verb"}, {Name: "hidden", Hidden: true}, {Name: "visible"}}
		},
	}
}

func TestKnownPath(t *testing.T) {
	leaf := &Node{}
	root := &Node{Sub: map[string]*Node{"--setup": {Sub: map[string]*Node{"mode": leaf}}}}
	e := syntheticEngine(map[string]*Node{"verb": root, "empty": nil})
	for _, tc := range []struct {
		words []string
		path  []string
		node  *Node
		rest  []string
	}{
		{},
		{words: []string{"unknown", "arg"}, rest: []string{"unknown", "arg"}},
		{words: []string{"alias", "--setup", "mode", "--flag", "value"},
			path: []string{"verb", "--setup", "mode"}, node: leaf, rest: []string{"--flag", "value"}},
		{words: []string{"verb", "unknown", "--setup"}, path: []string{"verb"}, node: root,
			rest: []string{"unknown", "--setup"}},
	} {
		path, node, rest := e.knownPath(tc.words)
		if !slices.Equal(path, tc.path) || node != tc.node || !slices.Equal(rest, tc.rest) {
			t.Errorf("knownPath(%v) = (%v, %p, %v), want (%v, %p, %v)",
				tc.words, path, node, rest, tc.path, tc.node, tc.rest)
		}
	}
	path, node, rest := e.knownPath([]string{"empty", "arg"})
	if !slices.Equal(path, []string{"empty"}) || node == nil || !slices.Equal(rest, []string{"arg"}) {
		t.Errorf("missing spec node = (%v, %v, %v), want an empty node for the resolved verb", path, node, rest)
	}
}

func TestFlagLookupAndRemainingFlags(t *testing.T) {
	values := func() []string { return []string{"second", "first"} }
	node := &Node{Flags: []Flag{{Name: "z", Values: values}, {Name: "bool", IsBool: true}, {Name: "a"}}}
	path := []string{"verb"}
	if f, ok := flagLookup(path, node, "--z"); !ok || f.Name != "z" || f.IsBool {
		t.Errorf("flagLookup(--z) = (%v, %v), want valued z", f, ok)
	}
	if !flagIsBool(path, node, "--bool") {
		t.Error("boolean flag was not recognized")
	}
	if got := flagValues(path, node, "--z"); !slices.Equal(got, values()) {
		t.Errorf("flag values = %v, want %v in domain order", got, values())
	}
	for _, word := range []string{"--bool", "--a", "--unknown"} {
		if got := flagValues(path, node, word); got != nil {
			t.Errorf("flagValues(%s) = %v, want nil", word, got)
		}
	}
	if _, ok := flagLookup(path, nil, "--z"); ok {
		t.Error("nil node resolved a flag")
	}
	if _, ok := flagLookup(path, node, "--unknown"); ok {
		t.Error("unknown flag resolved")
	}
	if got := remainingFlags(path, node, []string{"--bool"}); !slices.Equal(got, []string{"--a", "--z"}) {
		t.Errorf("remaining flags = %v, want [--a --z]", got)
	}
	if got := remainingFlags(path, nil, nil); got != nil {
		t.Errorf("nil node flags = %v, want nil", got)
	}
}

func TestLauncherFlagsRequireSubword(t *testing.T) {
	node := &Node{Flags: []Flag{{Name: "value", Values: func() []string { return []string{"candidate"} }}}}
	for name := range noComplete {
		path := []string{name}
		if _, ok := flagLookup(path, node, "--value"); ok {
			t.Errorf("%s resolved a bare launcher flag", name)
		}
		if got := remainingFlags(path, node, nil); got != nil {
			t.Errorf("%s bare launcher flags = %v, want nil", name, got)
		}
		if got := flagValues([]string{name, "ask"}, node, "--value"); !slices.Equal(got, []string{"candidate"}) {
			t.Errorf("%s ask flag values = %v, want [candidate]", name, got)
		}
	}
}

func TestPositionalInfoAndDomain(t *testing.T) {
	first := func() []string { return []string{"first"} }
	rest := func() []string { return []string{"rest"} }
	node := &Node{Flags: []Flag{{Name: "bool", IsBool: true}}, Args: []func() []string{first, nil}, Rest: rest}
	for _, tc := range []struct {
		words []string
		want  int
	}{
		{},
		{[]string{"--bool", "positional"}, 1},
		{[]string{"--value", "consumed", "positional"}, 1},
		{[]string{"--value=inline", "positional"}, 1},
		{[]string{"--unknown", "consumed", "one", "two"}, 2},
		{[]string{"--value", "--bool", "positional"}, 1},
	} {
		if got := positionalInfo([]string{"verb"}, node, tc.words); got != tc.want {
			t.Errorf("positionalInfo(%v) = %d, want %d", tc.words, got, tc.want)
		}
	}
	if got := positionalDomain(node, 0)(); !slices.Equal(got, first()) {
		t.Errorf("first domain = %v, want %v", got, first())
	}
	if positionalDomain(node, 1) != nil || positionalDomain(nil, 0) != nil {
		t.Error("nil positional domain was not preserved")
	}
	if got := positionalDomain(node, 2)(); !slices.Equal(got, rest()) {
		t.Errorf("rest domain = %v, want %v", got, rest())
	}
}

func TestEngineCandidatesPreserveOrder(t *testing.T) {
	values := func() []string { return []string{"second", "first"} }
	node := &Node{Flags: []Flag{{Name: "z", Values: values}, {Name: "bool", IsBool: true}, {Name: "a"}},
		Args: []func() []string{values}, Rest: func() []string { return []string{"rest"} }}
	e := syntheticEngine(map[string]*Node{"verb": node})
	for _, tc := range []struct {
		words []string
		want  []string
	}{
		{nil, []string{"verb", "visible", "help"}},
		{[]string{""}, []string{"verb", "visible", "help"}},
		{[]string{"h"}, []string{"hidden", "help"}},
		{[]string{"alias", ""}, []string{"second", "first"}},
		{[]string{"verb", "--z", ""}, []string{"second", "first"}},
		{[]string{"verb", "--z=fi"}, []string{"--z=first"}},
		{[]string{"verb", "--"}, []string{"--a", "--bool", "--z"}},
		{[]string{"verb", "--bool", "--"}, []string{"--a", "--z"}},
		{[]string{"verb", "first", ""}, []string{"rest", "--a", "--bool", "--z"}},
		{[]string{"verb", "first", "r"}, []string{"rest"}},
		{[]string{"unknown", ""}, nil},
	} {
		if got := e.Candidates(tc.words); !slices.Equal(got, tc.want) {
			t.Errorf("Candidates(%v) = %v, want %v", tc.words, got, tc.want)
		}
	}
}

func TestCandidatesAfterInlineFlagValue(t *testing.T) {
	e := syntheticEngine(map[string]*Node{
		"agents": {Sub: map[string]*Node{
			"ask": {Flags: []Flag{
				{Name: "agent", Values: func() []string { return []string{"codex"} }},
				{Name: "verbose", IsBool: true},
			}},
		}},
	})
	words := []string{"agents", "ask", "--agent=codex", ""}
	want := []string{"--verbose"}
	if got := e.Candidates(words); !slices.Equal(got, want) {
		t.Fatalf("Candidates(%q) = %v, want remaining flags %v", words, got, want)
	}
}

func TestCandidatesSuppressTypedInlineFlag(t *testing.T) {
	e := syntheticEngine(map[string]*Node{
		"usage": {Flags: []Flag{{Name: "format"}, {Name: "format-style"}}},
	})
	words := []string{"usage", "--format=json", "--format"}
	want := []string{"--format-style"}
	if got := e.Candidates(words); !slices.Equal(got, want) {
		t.Fatalf("Candidates(%q) = %v, want only untyped flag %v", words, got, want)
	}
}

func TestEmptyPrefixSubcommandsSorted(t *testing.T) {
	sub := map[string]*Node{}
	for _, w := range []string{"merge", "clean", "list", "create", "clean-all", "alpha", "zeta"} {
		sub[w] = &Node{}
	}
	e := syntheticEngine(map[string]*Node{"verb": {Sub: sub}})
	want := []string{"alpha", "clean", "clean-all", "create", "list", "merge", "zeta"}
	for range 20 {
		if got := e.Candidates([]string{"verb", ""}); !slices.Equal(got, want) {
			t.Fatalf("Candidates(verb \"\") = %v, want sorted %v", got, want)
		}
	}
}
