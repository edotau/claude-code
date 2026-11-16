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

type entry struct {
	ID            string            `json:"id"`
	Version       string            `json:"version,omitempty"` // dash-separated version a dotted spelling turns into dots
	ResponsesOnly bool              `json:"responses_only,omitempty"`
	ChatOnly      bool              `json:"chat_only,omitempty"`
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
	Models    []entry             `json:"models"`

	byID    map[string]entry
	byWire  map[string]string // every known spelling of every model -> canonical id
	aliasOf map[string]string // canonical id -> the alias it currently backs
}

// load parses the embedded catalog once; a malformed embed is a build defect, so it panics (tests cover it).
var load = sync.OnceValue(func() *catalog {
	var c catalog
	if err := json.Unmarshal(catalogJSON, &c); err != nil {
		panic("models: catalog.json: " + err.Error())
	}
	c.byID = map[string]entry{}
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
	if rule == "databricks-serving" {
		id = c.undated(id) // serving endpoints carry no date stamp
	}
	if sp.Dotted {
		id = dotVersion(id, c.byID[id].Version)
	}
	return sp.Prefix + id
}

// undated drops a trailing -YYYYMMDD from id when the undated id is in the catalog.
func (c *catalog) undated(id string) string {
	if i := len(id) - 9; i > 0 && id[i] == '-' && strings.Trim(id[i+1:], "0123456789") == "" {
		if _, ok := c.byID[id[:i]]; ok {
			return id[:i]
		}
	}
	return id
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

// WireOrOriginal is id's wire spelling under rule when it resolves to a known model, else id unchanged.
func WireOrOriginal(rule, id string) string {
	if canonical, _, ok := Resolve(id); ok {
		return Wire(rule, canonical)
	}
	return id
}

// Alias resolves an alias (opus, sonnet, haiku, fable) to the canonical id it currently backs.
func Alias(name string) (canonical string, ok bool) {
	canonical, ok = load().Aliases[strings.ToLower(strings.TrimSpace(name))]
	return canonical, ok
}

// AliasOf returns the alias a canonical id currently backs, or "" when none.
func AliasOf(canonical string) string { return load().aliasOf[canonical] }

// Limits resolves id like Resolve and returns the catalog limits
func Limits(id string) (TokenLimits, bool) {
	canonical, _, ok := Resolve(id)
	if !ok {
		return TokenLimits{}, false
	}
	l := load().byID[canonical].Limits
	return l, l != (TokenLimits{})
}

var wirePrefixes = [...]string{"databricks-", "system.ai.", "system.ai-"}

// stripWirePrefix cuts the first wire prefix s carries; ok is false (rest == s) when it has none.
func stripWirePrefix(s string) (rest string, ok bool) {
	for _, p := range wirePrefixes {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return rest, true
		}
	}
	return s, false
}

// SameModel reports whether a and b name one model across spellings: case, [1m], wire prefixes, aliases, short forms.
func SameModel(a, b string) bool { return ModelKey(a) == ModelKey(b) }

// ModelKey is id's spelling-free comparison key: the canonical id when it resolves, else the lowercased bare id.
func ModelKey(id string) string {
	if canonical, _, ok := Resolve(id); ok {
		return canonical
	}
	rest, _ := stripWirePrefix(Strip1M(strings.ToLower(strings.TrimSpace(id))))
	return rest
}

// Resolve maps an alias, short Claude form, canonical id or any wire spelling to the canonical id
func Resolve(s string) (canonical string, oneM bool, ok bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	marked := Has1M(s)
	s = Strip1M(s)
	c := load()
	id, found := c.Aliases[s]
	if !found {
		if id = c.lookup(s); id == "" {
			id = parseClaude(s)
		}
	}
	if id == "" {
		return "", false, false
	}
	return id, marked && Keeps1M(id+marker1M), true
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
	if rest, ok := stripWirePrefix(s); ok {
		return c.byWire[rest]
	}
	return ""
}

// parseClaude canonicalizes any Claude spelling (dotted, short, prefixed, dated) the catalog does not list.
func parseClaude(s string) string {
	if hasFamilyDash(s) {
		s = "claude-" + s
	}
	m := claudeRE().FindStringSubmatch(s)
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
