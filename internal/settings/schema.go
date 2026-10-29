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
	Description string
	Env         bool
	pattern     *regexp.Regexp // alternate accepted string form (theme "custom:<slug>")
}

// schemaType is a JSON-Schema "type": one name or a list; the first non-null name wins.
type schemaType string

func (t *schemaType) UnmarshalJSON(b []byte) error {
	var one string
	var many []string
	if json.Unmarshal(b, &one) == nil {
		*t = schemaType(one)
		return nil
	}
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	for _, m := range many {
		if m != "null" {
			*t = schemaType(m)
			return nil
		}
	}
	return nil
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
	Type        schemaType   `json:"type"`
	Enum        schemaEnum   `json:"enum"`
	Minimum     *float64     `json:"minimum"`
	Pattern     string       `json:"pattern"`
	Description string       `json:"description"`
	AnyOf       []schemaNode `json:"anyOf"`
}

// flatten folds an anyOf node's branches into one node: enum and type from the first typed branch, pattern from any.
func (n schemaNode) flatten() schemaNode {
	for _, b := range n.AnyOf {
		if n.Type == "" {
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

func (n schemaNode) key(name string, env bool) Key {
	n = n.flatten()
	k := Key{Name: name, Enum: slices.Clone(n.Enum), Min: n.Minimum, Description: n.Description, Env: env}
	switch {
	case len(n.Enum) > 0:
		k.Kind = KindEnum
	case n.Type == "boolean":
		k.Kind = KindBool
	case n.Type == "integer":
		k.Kind = KindInt
	case n.Type == "number":
		k.Kind = KindNumber
	case n.Type == "object":
		k.Kind = KindObject
	case n.Type == "array":
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
		if k.Min != nil && n < *k.Min {
			return fmt.Errorf("%s: must be >= %v, got %v", k.Name, *k.Min, n)
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
	case KindObject:
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return fmt.Errorf("%s: want a JSON object", k.Name)
		}
	case KindArray:
		var a []json.RawMessage
		if json.Unmarshal(raw, &a) != nil {
			return fmt.Errorf("%s: want a JSON array", k.Name)
		}
	default:
		return fmt.Errorf("%s: unknown kind", k.Name)
	}
	return nil
}
