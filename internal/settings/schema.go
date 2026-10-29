package settings

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
)

//go:embed claude-code-settings.schema.json
var schemaJSON []byte

// Key is one setting (or env variable) the schemastore schema documents.
type Key struct {
	Name        string
	Kind        Kind
	Enum        []string
	Min         *float64
	Max         *float64
	ExMin       *float64 // exclusive bounds
	ExMax       *float64
	Description string
	Env         bool
	pattern     *regexp.Regexp // alternate accepted string form (theme "custom:<slug>")
	alts        []Key          // union branches (anyOf, oneOf, a type list): valid when any accepts
	items       *Key           // array element schema
	props       map[string]Key // object property schemas
}

// schemaType is a JSON-Schema "type": one name or a list, kept whole so unions validate every branch.
type schemaType []string

func (t *schemaType) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*t = schemaType{one}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(t))
}

// first is the first non-null type name, "" when there is none.
func (t schemaType) first() string {
	for _, m := range t {
		if m != "null" {
			return m
		}
	}
	return ""
}

// branches lists the non-null type names.
func (t schemaType) branches() []string {
	var out []string
	for _, m := range t {
		if m != "null" {
			out = append(out, m)
		}
	}
	return out
}

// schemaEnum keeps the string members of an "enum" and ignores others (null, numbers).
type schemaEnum []string

func (e *schemaEnum) UnmarshalJSON(b []byte) error {
	var all []any
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for _, v := range all {
		if s, ok := v.(string); ok {
			*e = append(*e, s)
		}
	}
	return nil
}

type schemaNode struct {
	Type        schemaType            `json:"type"`
	Enum        schemaEnum            `json:"enum"`
	Minimum     *float64              `json:"minimum"`
	Maximum     *float64              `json:"maximum"`
	ExMinimum   *float64              `json:"exclusiveMinimum"`
	ExMaximum   *float64              `json:"exclusiveMaximum"`
	Pattern     string                `json:"pattern"`
	Description string                `json:"description"`
	AnyOf       []schemaNode          `json:"anyOf"`
	OneOf       []schemaNode          `json:"oneOf"`
	Items       *schemaNode           `json:"items"`
	Properties  map[string]schemaNode `json:"properties"`
}

// flatten folds an anyOf node's branches into one node: enum and type from the first typed branch, pattern from any.
func (n schemaNode) flatten() schemaNode {
	for _, b := range slices.Concat(n.AnyOf, n.OneOf) {
		if n.Type.first() == "" {
			n.Type = b.Type
		}
		if len(n.Enum) == 0 {
			n.Enum = b.Enum
		}
		if n.Pattern == "" {
			n.Pattern = b.Pattern
		}
	}
	return n
}

// unionBranches lists the nodes a value may match; fewer than two means the node is not a union.
func (n schemaNode) unionBranches() []schemaNode {
	out := slices.Concat(n.AnyOf, n.OneOf)
	if len(out) == 0 {
		for _, t := range n.Type.branches() {
			out = append(out, schemaNode{Type: schemaType{t}, Enum: n.Enum, Minimum: n.Minimum, Maximum: n.Maximum,
				ExMinimum: n.ExMinimum, ExMaximum: n.ExMaximum, Pattern: n.Pattern, Items: n.Items, Properties: n.Properties})
		}
	}
	return out
}

func (n schemaNode) key(name string, env bool) Key {
	branches := n.unionBranches()
	n = n.flatten()
	k := Key{Name: name, Enum: slices.Clone(n.Enum), Min: n.Minimum, Max: n.Maximum, ExMin: n.ExMinimum,
		ExMax: n.ExMaximum, Description: n.Description, Env: env}
	if len(branches) > 1 {
		for _, b := range branches {
			k.alts = append(k.alts, b.key(name, env))
		}
	}
	if n.Items != nil {
		it := n.Items.key(name+"[]", env)
		k.items = &it
	}
	if len(n.Properties) > 0 {
		k.props = make(map[string]Key, len(n.Properties))
		for pn, pv := range n.Properties {
			k.props[pn] = pv.key(name+"."+pn, env)
		}
	}
	typ := n.Type.first()
	switch {
	case len(n.Enum) > 0:
		k.Kind = KindEnum
	case typ == "boolean":
		k.Kind = KindBool
	case typ == "integer":
		k.Kind = KindInt
	case typ == "number":
		k.Kind = KindNumber
	case typ == "object":
		k.Kind = KindObject
	case typ == "array":
		k.Kind = KindArray
	default:
		k.Kind = KindString
	}
	if n.Pattern != "" {
		k.pattern, _ = regexp.Compile(n.Pattern)
	}
	return k
}

var schemaIndex = sync.OnceValue(func() map[string]Key {
	var doc struct {
		Properties map[string]schemaNode `json:"properties"`
	}
	if err := json.Unmarshal(schemaJSON, &doc); err != nil {
		panic(fmt.Sprintf("settings: embedded schema: %v", err))
	}
	m := make(map[string]Key, len(doc.Properties))
	for name, n := range doc.Properties {
		m[name] = n.key(name, false)
	}
	var full struct {
		Properties struct {
			Env struct {
				Properties map[string]schemaNode `json:"properties"`
			} `json:"env"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaJSON, &full); err != nil {
		panic(fmt.Sprintf("settings: embedded schema env: %v", err))
	}
	for name, n := range full.Properties.Env.Properties {
		if _, clash := m[name]; !clash {
			m[name] = n.key(name, true)
		}
	}
	return m
})

// SchemaKey returns the schema entry for a settings key or an env variable name.
func SchemaKey(name string) (Key, bool) {
	k, ok := schemaIndex()[name]
	return k, ok
}

// SchemaKeys returns every schema entry sorted by name.
func SchemaKeys() []Key {
	m := schemaIndex()
	keys := make([]Key, 0, len(m))
	for _, k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b Key) int { return strings.Compare(a.Name, b.Name) })
	return keys
}

// Validate checks raw against the key's kind, enum and minimum.
func (k Key) Validate(raw json.RawMessage) error {
	if strings.TrimSpace(string(raw)) == "null" {
		return fmt.Errorf("%s: want a value, got null", k.Name)
	}
	if len(k.alts) > 0 {
		var errs []string
		for _, a := range k.alts {
			err := a.Validate(raw)
			if err == nil {
				return nil
			}
			errs = append(errs, err.Error())
		}
		return fmt.Errorf("%s: matches no accepted form (%s)", k.Name, strings.Join(errs, "; "))
	}
	switch k.Kind {
	case KindBool:
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return fmt.Errorf("%s: want a boolean", k.Name)
		}
	case KindInt, KindNumber:
		var n float64
		if json.Unmarshal(raw, &n) != nil {
			return fmt.Errorf("%s: want a number", k.Name)
		}
		if k.Kind == KindInt && n != float64(int64(n)) {
			return fmt.Errorf("%s: want an integer, got %v", k.Name, n)
		}
		if err := k.checkRange(n); err != nil {
			return err
		}
	case KindEnum:
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return fmt.Errorf("%s: want a string", k.Name)
		}
		if k.pattern != nil && k.pattern.MatchString(s) {
			return nil
		}
		if !slices.Contains(k.Enum, s) {
			return fmt.Errorf("%s: want one of %v, got %q", k.Name, k.Enum, s)
		}
	case KindString:
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return fmt.Errorf("%s: want a string", k.Name)
		}
		if k.pattern != nil && !k.pattern.MatchString(s) {
			return fmt.Errorf("%s: %q does not match %s", k.Name, s, k.pattern)
		}
	case KindObject:
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return fmt.Errorf("%s: want a JSON object", k.Name)
		}
		for pn, pv := range m {
			if sub, ok := k.props[pn]; ok {
				if err := sub.Validate(pv); err != nil {
					return err
				}
			}
		}
	case KindArray:
		var a []json.RawMessage
		if json.Unmarshal(raw, &a) != nil {
			return fmt.Errorf("%s: want a JSON array", k.Name)
		}
		if k.items != nil {
			for _, it := range a {
				if err := k.items.Validate(it); err != nil {
					return err
				}
			}
		}
	default:
		return fmt.Errorf("%s: unknown kind", k.Name)
	}
	return nil
}

// checkRange enforces minimum, maximum and both exclusive bounds.
func (k Key) checkRange(n float64) error {
	switch {
	case k.Min != nil && n < *k.Min:
		return fmt.Errorf("%s: must be >= %v, got %v", k.Name, *k.Min, n)
	case k.ExMin != nil && n <= *k.ExMin:
		return fmt.Errorf("%s: must be > %v, got %v", k.Name, *k.ExMin, n)
	case k.Max != nil && n > *k.Max:
		return fmt.Errorf("%s: must be <= %v, got %v", k.Name, *k.Max, n)
	case k.ExMax != nil && n >= *k.ExMax:
		return fmt.Errorf("%s: must be < %v, got %v", k.Name, *k.ExMax, n)
	}
	return nil
}
