package models

import (
	_ "embed"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
)

//go:embed catalog.json
var catalogJSON []byte

// Entry is one catalog model: canonical id, display name and capabilities.
type Entry struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Version       string            `json:"version,omitempty"` // dash-separated version a dotted spelling turns into dots
	Thinking      bool              `json:"thinking,omitempty"`
	ResponsesOnly bool              `json:"responses_only,omitempty"`
	WireOverride  map[string]string `json:"wire,omitempty"`  // spelling rule -> exact wire id, for exceptions
	Limits        TokenLimits       `json:"limits,omitzero"` // documented token limits; a zero field is unknown
}

// TokenLimits are a model's documented context window and max output, in tokens; zero means unknown, never a guess.
type TokenLimits struct {
	Context int `json:"context,omitempty"`
	Output  int `json:"output,omitempty"`
}

// spelling is one named wire-spelling rule: an optional id prefix and whether the version uses dots.
type spelling struct {
	Prefix string `json:"prefix,omitempty"`
	Dotted bool   `json:"dotted,omitempty"`
}

type catalog struct {
	Aliases   map[string]string   `json:"aliases"`
	Spellings map[string]spelling `json:"spellings"`
	Models    []Entry             `json:"models"`

	byID    map[string]Entry
	byWire  map[string]string // every known spelling of every model -> canonical id
	aliasOf map[string]string // canonical id -> the alias it currently backs
}

// load parses the embedded catalog once; a malformed embed is a build defect, so it panics (tests cover it).
var load = sync.OnceValue(func() *catalog {
	var c catalog
	if err := json.Unmarshal(catalogJSON, &c); err != nil {
		panic("models: catalog.json: " + err.Error())
	}
	c.byID = map[string]Entry{}
	c.aliasOf = map[string]string{}
	for _, m := range c.Models {
		c.byID[m.ID] = m
	}
	for a, id := range c.Aliases {
		c.aliasOf[id] = a
	}
	c.byWire = map[string]string{}
	for _, m := range c.Models {
		for rule := range c.Spellings {
			c.byWire[strings.ToLower(c.wire(rule, m.ID))] = m.ID
		}
		for _, w := range m.WireOverride {
			c.byWire[strings.ToLower(w)] = m.ID
		}
		c.byWire[m.ID] = m.ID
	}
	return &c
})

// wire applies a spelling rule to a canonical id; an unknown rule returns the id unchanged.
func (c *catalog) wire(rule, id string) string {
	sp, ok := c.Spellings[rule]
	if !ok {
		return id
	}
	if w, ok := c.byID[id].WireOverride[rule]; ok {
		return w
	}
	if sp.Dotted {
		id = dotVersion(id, c.byID[id].Version)
	}
	return sp.Prefix + id
}

// dotVersion turns the dashed version inside id into dots; ids with no declared or parseable version are untouched.
func dotVersion(id, version string) string {
	if version == "" {
		if _, maj, min, ok := ParseClaudeID(id); ok && min > 0 {
			version = strconv.Itoa(maj) + "-" + strconv.Itoa(min)
		}
	}
	if version == "" || !strings.Contains(version, "-") {
		return id
	}
	dotted := strings.Replace(version, "-", ".", 1)
	for from := 0; ; {
		i := strings.Index(id[from:], version)
		if i < 0 {
			return id
		}
		i += from
		end := i + len(version)
		if end == len(id) || id[end] == '-' {
			return id[:i] + dotted + id[end:]
		}
		from = i + 1
	}
}

// Wire builds the wire id of canonical for a named spelling rule (anthropic, databricks-serving, ...).
func Wire(rule, canonical string) string { return load().wire(rule, canonical) }

// Alias resolves a tier alias (opus, sonnet, haiku, fable) to the canonical id it currently backs.
func Alias(name string) (canonical string, ok bool) {
	canonical, ok = load().Aliases[strings.ToLower(strings.TrimSpace(name))]
	return canonical, ok
}

// AliasOf returns the alias a canonical id currently backs, or "" when none.
func AliasOf(canonical string) string { return load().aliasOf[canonical] }

// Lookup returns the catalog entry (display name, capabilities) of a canonical id.
func Lookup(canonical string) (Entry, bool) {
	e, ok := load().byID[canonical]
	return e, ok
}

// Limits resolves id like Resolve and returns the catalog limits; false when id is not a catalog model or
// the catalog documents neither limit.
func Limits(id string) (TokenLimits, bool) {
	canonical, _, ok := Resolve(id)
	if !ok {
		return TokenLimits{}, false
	}
	l := load().byID[canonical].Limits
	return l, l != (TokenLimits{})
}

// Catalog returns every catalog model in file order.
func Catalog() []Entry { return append([]Entry(nil), load().Models...) }

// wirePrefixes carry no model identity on any wire spelling.
var wirePrefixes = [...]string{"databricks-", "system.ai.", "system.ai-"}

// Resolve maps an alias, short Claude form, canonical id or any wire spelling to the canonical id; oneM is true
// when the input carried the client-only [1m] marker.
func Resolve(s string) (canonical string, oneM bool, ok bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	oneM = Has1M(s)
	s = Strip1M(s)
	c := load()
	if id, found := c.Aliases[s]; found {
		return id, oneM, true
	}
	if id := c.lookup(s); id != "" {
		return id, oneM, true
	}
	if id := parseClaude(s); id != "" {
		return id, oneM, true
	}
	return "", oneM, false
}

// lookup tries s, then its last "/" segment (opencode, nested ids), then s without a known wire prefix.
func (c *catalog) lookup(s string) string {
	if id, ok := c.byWire[s]; ok {
		return id
	}
	if _, after, ok := strings.CutLast(s, "/"); ok {
		if id := c.lookup(after); id != "" {
			return id
		}
	}
	for _, p := range wirePrefixes {
		if rest, ok := strings.CutPrefix(s, p); ok {
			if id, ok := c.byWire[rest]; ok {
				return id
			}
		}
	}
	return ""
}

// parseClaude canonicalizes any Claude spelling (dotted, short, prefixed, dated) the catalog does not list.
func parseClaude(s string) string {
	if hasFamilyDash(s) {
		s = "claude-" + s
	}
	m := claudeRE.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	id := "claude-" + m[1] + "-" + m[2]
	switch {
	case m[3] != "" && len(m[3]) <= 2:
		id += "-" + m[3]
	case m[3] != "":
		m[4] = m[3] // a long trailing group is the date stamp
	}
	if m[4] != "" {
		id += "-" + m[4]
	}
	return id
}
