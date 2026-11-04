package settings

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
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
	patternErr  error          // a pattern that failed to compile: Validate refuses rather than skip the check
	alts        []Key          // union branches (anyOf, oneOf, a type list): valid when any accepts
	items       *Key           // array element schema
	props       map[string]Key // object property schemas
	required    []string
	constVal    json.RawMessage
	noExtra     bool // additionalProperties: false
	addl        *Key // additionalProperties schema applied to unlisted properties
	ref         bool // uses $ref, which is not resolved: the picker skips it
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
	Required    []string              `json:"required"`
	Const       json.RawMessage       `json:"const"`
	AddlProps   json.RawMessage       `json:"additionalProperties"`
	Ref         string                `json:"$ref"`
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
	k.required, k.constVal = n.Required, n.Const
	k.noExtra = strings.TrimSpace(string(n.AddlProps)) == "false"
	k.addl = n.addlKey(name, env)
	k.ref = n.Ref != "" || k.childRef()
	k.Kind = n.kind()
	if n.Pattern != "" {
		k.pattern, k.patternErr = regexp.Compile(n.Pattern)
	}
	return k
}

// addlKey is the Key of an object-valued additionalProperties schema, or nil.
func (n schemaNode) addlKey(name string, env bool) *Key {
	if !strings.HasPrefix(strings.TrimSpace(string(n.AddlProps)), "{") {
		return nil
	}
	var sub schemaNode
	if json.Unmarshal(n.AddlProps, &sub) != nil {
		return nil
	}
	a := sub.key(name+".*", env)
	return &a
}

// childRef reports whether any item, alternative, property or additionalProperties child holds a $ref.
func (k Key) childRef() bool {
	if (k.items != nil && k.items.ref) || (k.addl != nil && k.addl.ref) {
		return true
	}
	for _, c := range slices.Concat(k.alts, slices.Collect(maps.Values(k.props))) {
		if c.ref {
			return true
		}
	}
	return false
}

// kind maps the node's enum/type to a Kind; anything else is a string.
func (n schemaNode) kind() Kind {
	if len(n.Enum) > 0 {
		return KindEnum
	}
	switch n.Type.first() {
	case "boolean":
		return KindBool
	case "integer":
		return KindInt
	case "number":
		return KindNumber
	case "object":
		return KindObject
	case "array":
		return KindArray
	}
	return KindString
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

// SubField returns the Field for a scalar property of an object setting (parent.sub); ok is false for anything else.
func SubField(parent, sub string) (Field, bool) {
	pk, ok := SchemaKey(parent)
	sk, found := pk.props[sub]
	if !ok || !found || sk.Kind == KindObject || sk.Kind == KindArray || sk.ref {
		return Field{}, false
	}
	f := Field{Key: parent + "." + sub, Kind: sk.Kind, Values: sk.Enum, Source: "schema"}
	if sk.Min != nil {
		f.Min = int(*sk.Min)
	}
	return f, true
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
	if k.patternErr != nil {
		return fmt.Errorf("%s: schema pattern does not compile: %w", k.Name, k.patternErr)
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return fmt.Errorf("%s: want a value, got null", k.Name)
	}
	if len(k.constVal) > 0 {
		if a, errA := compactStr(raw); errA != nil || a != mustCompact(k.constVal) {
			return fmt.Errorf("%s: must be %s", k.Name, k.constVal)
		}
	}
	if len(k.alts) > 0 {
		return k.validateAlternatives(raw)
	}
	return k.validateKind(raw)
}

func (k Key) validateAlternatives(raw json.RawMessage) error {
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

func (k Key) validateKind(raw json.RawMessage) error {
	switch k.Kind {
	case KindBool:
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return fmt.Errorf("%s: want a boolean", k.Name)
		}
	case KindInt, KindNumber:
		return k.validateNumber(raw)
	case KindEnum:
		return k.validateEnum(raw)
	case KindString:
		return k.validateString(raw)
	case KindObject:
		return k.validateObject(raw)
	case KindArray:
		return k.validateArray(raw)
	default:
		return fmt.Errorf("%s: unknown kind", k.Name)
	}
	return nil
}

func (k Key) validateNumber(raw json.RawMessage) error {
	var n float64
	if json.Unmarshal(raw, &n) != nil {
		return fmt.Errorf("%s: want a number", k.Name)
	}
	if k.Kind == KindInt && n != float64(int64(n)) {
		return fmt.Errorf("%s: want an integer, got %v", k.Name, n)
	}
	return k.checkRange(n)
}

func (k Key) validateEnum(raw json.RawMessage) error {
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
	return nil
}

func (k Key) validateString(raw json.RawMessage) error {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return fmt.Errorf("%s: want a string", k.Name)
	}
	if k.pattern != nil && !k.pattern.MatchString(s) {
		return fmt.Errorf("%s: %q does not match %s", k.Name, s, k.pattern)
	}
	return nil
}

func (k Key) validateObject(raw json.RawMessage) error {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return fmt.Errorf("%s: want a JSON object", k.Name)
	}
	for _, r := range k.required {
		if _, ok := m[r]; !ok {
			return fmt.Errorf("%s: missing required %q", k.Name, r)
		}
	}
	for pn, pv := range m {
		if err := k.validateProperty(pn, pv); err != nil {
			return err
		}
	}
	return nil
}

func (k Key) validateProperty(pn string, pv json.RawMessage) error {
	if _, ok := k.props[pn]; !ok && k.noExtra {
		return fmt.Errorf("%s: unknown property %q", k.Name, pn)
	}
	sub, ok := k.props[pn]
	if !ok && k.addl != nil && !k.addl.ref {
		sub, ok = *k.addl, true
	}
	if ok {
		return sub.Validate(pv)
	}
	return nil
}

func (k Key) validateArray(raw json.RawMessage) error {
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

// mustCompact is compactStr for a schema constant, which is always valid JSON.
func mustCompact(raw json.RawMessage) string {
	c, _ := compactStr(raw)
	return c
}

// HasRef reports a schema entry using $ref; its constraints are unresolved, so the picker does not offer it.
func (k Key) HasRef() bool { return k.ref }
