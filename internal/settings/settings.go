// Package settings seeds a missing settings.json and resets it on the owner's request; nothing else writes it.
package settings

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

//go:embed template.json
var template []byte

// Template is the embedded settings template, read-only: the harness keys a fresh settings.json starts from.
func Template() []byte { return template }

// Render is the template plus the hook registry block; only SeedSettings and ResetSettings may call it.
func Render() ([]byte, error) {
	o, err := parseObject(Template())
	if err != nil {
		return nil, fmt.Errorf("settings template: %w", err)
	}
	hooks, err := settingsJSON()
	if err != nil {
		return nil, err
	}
	top, err := schemaProps(schemaJSON)
	if err != nil {
		return nil, err
	}
	events, err := schemaProps(top.vals["hooks"])
	if err != nil {
		return nil, err
	}
	h, err := parseObject(hooks)
	if err != nil {
		return nil, err
	}
	h.orderBy(events.keys)
	if hooks, err = h.marshal(); err != nil {
		return nil, err
	}
	o.set("hooks", hooks)
	o.orderBy(top.keys)
	return o.marshal()
}

// Format reorders data to schemastore order: schema keys first, env keys and string lists sorted, hook lists kept.
func Format(data []byte) ([]byte, error) {
	out, _, err := format(data)
	return out, err
}

func format(data []byte) ([]byte, string, error) {
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, "", located(data, err)
	}
	if _, err := parseObject(raw); err != nil {
		return nil, "", err
	}
	path := ""
	out, err := formatValue(raw, schemaJSON, "", &path)
	if path == "" && !bytes.Equal(data, out) {
		path = "(whitespace)"
	}
	return out, path, err
}

// FormatFile rewrites file with Format when its bytes differ and reports whether it did.
func FormatFile(file string) (changed bool, err error) {
	file, err = filepath.EvalSymlinks(file)
	if err != nil {
		return false, err
	}
	err = paths.WithFileLock(file, func() error {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		out, err := Format(data)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if bytes.Equal(data, out) {
			return nil
		}
		fi, err := os.Stat(file)
		if err != nil {
			return err
		}
		changed = true
		return paths.AtomicWrite(file, out, fi.Mode().Perm())
	})
	return changed && err == nil, err
}

// Unformatted names the first path where data differs from its Format, "" when it is already formatted.
func Unformatted(data []byte) (string, error) {
	_, path, err := format(data)
	return path, err
}

// resolveRef follows local JSON pointers; non-local references stay unchanged.
func resolveRef(node json.RawMessage) json.RawMessage {
	for range 8 {
		var ref struct {
			Ref string `json:"$ref"`
		}
		if json.Unmarshal(node, &ref) != nil || !strings.HasPrefix(ref.Ref, "#/") {
			return node
		}
		node = schemaJSON
		for seg := range strings.SplitSeq(ref.Ref[2:], "/") {
			var props map[string]json.RawMessage
			if json.Unmarshal(node, &props) != nil {
				return nil
			}
			node = props[strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")]
		}
	}
	return node
}

func formatValue(raw, node json.RawMessage, path string, first *string) (json.RawMessage, error) {
	var schema map[string]json.RawMessage
	node = resolveRef(node)
	_ = json.Unmarshal(node, &schema)
	switch bytes.TrimSpace(raw)[0] {
	case '{':
		return formatObject(raw, node, schema["additionalProperties"], path, first)
	case '[':
		return formatArray(raw, schema["items"], path, first)
	}
	return raw, nil
}

func formatObject(raw, node, additional json.RawMessage, path string, first *string) (json.RawMessage, error) {
	o, err := parseObject(raw)
	if err != nil {
		return nil, err
	}
	props, err := schemaProps(node)
	if err != nil {
		props = &object{}
	}
	keys := slices.Clone(o.keys)
	o.orderBy(props.keys)
	if path == "env" {
		slices.Sort(o.keys)
	}
	for i, k := range keys {
		sub := strings.TrimPrefix(path+"."+k, ".")
		if *first == "" && k != o.keys[i] {
			*first = sub
		}
		child := props.vals[k]
		if child == nil {
			child = additional
		}
		if o.vals[k], err = formatValue(o.vals[k], child, sub, first); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
	}
	return o.marshal()
}

func formatArray(raw, node json.RawMessage, path string, first *string) (json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	values := make(map[string]string, len(items))
	allStrings := true
	for _, item := range items {
		if bytes.TrimSpace(item)[0] != '"' {
			allStrings = false
			break
		}
		var value string
		_ = json.Unmarshal(item, &value)
		values[string(item)] = value
	}
	original := slices.Clone(items)
	if allStrings {
		slices.SortStableFunc(items, func(a, b json.RawMessage) int {
			x, y := values[string(a)], values[string(b)]
			return compareStrings(x, y)
		})
	}
	for i, item := range items {
		sub := fmt.Sprintf("%s[%d]", path, i)
		if allStrings && *first == "" && !bytes.Equal(item, original[i]) {
			*first = sub
		}
		var err error
		if items[i], err = formatValue(item, node, sub, first); err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
	}
	return textutil.MarshalNoEscape(items)
}

func compareStrings(x, y string) int {
	if (x == "$defaults") != (y == "$defaults") {
		if x == "$defaults" {
			return -1
		}
		return 1
	}
	if c := strings.Compare(strings.ToLower(x), strings.ToLower(y)); c != 0 {
		return c
	}
	return strings.Compare(x, y)
}

// schemaProps is a schema node's "properties" object, in schemastore's key order.
func schemaProps(node json.RawMessage) (*object, error) {
	n, err := parseObject(node)
	if err != nil {
		return nil, fmt.Errorf("settings schema: %w", err)
	}
	if n.vals["properties"] != nil {
		props, err := parseObject(n.vals["properties"])
		if err != nil {
			return nil, fmt.Errorf("settings schema: %w", err)
		}
		return props, nil
	}
	var branches []json.RawMessage
	_ = json.Unmarshal(n.vals["anyOf"], &branches)
	if len(branches) == 0 {
		_ = json.Unmarshal(n.vals["oneOf"], &branches)
	}
	props := &object{vals: map[string]json.RawMessage{}}
	for _, branch := range branches {
		child, err := schemaProps(resolveRef(branch))
		if err != nil {
			continue
		}
		for _, k := range child.keys {
			if props.vals[k] == nil {
				props.set(k, child.vals[k])
			}
		}
	}
	return props, nil
}

// orderBy moves the keys ref lists to ref's order ahead of the rest, which keep their order.
func (o *object) orderBy(ref []string) {
	rank := make(map[string]int, len(ref))
	for i, k := range ref {
		rank[k] = i
	}
	slices.SortStableFunc(o.keys, func(a, b string) int {
		ra, okA := rank[a]
		rb, okB := rank[b]
		switch {
		case okA && okB:
			return ra - rb
		case okA:
			return -1
		case okB:
			return 1
		}
		return 0
	})
}

// object is a JSON object that remembers key order, so the render keeps the template's layout.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func parseObject(b []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	o := &object{vals: map[string]json.RawMessage{}}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		o.set(tok.(string), raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *object) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// marshal writes 2-space indented JSON without HTML escaping (hook commands carry & and >).
func (o *object) marshal() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{")
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteString(",")
		}
		key, _ := textutil.MarshalNoEscape(k)
		buf.WriteString("\n  ")
		buf.Write(key)
		buf.WriteString(": ")
		if err := json.Indent(&buf, bytes.TrimSpace(o.vals[k]), "  ", "  "); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
	}
	if len(o.keys) > 0 {
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

// FilterObject keeps the entries of the JSON object raw that keep accepts (optionally rewriting the value), in order.
func FilterObject(raw []byte, keep func(key string, val json.RawMessage) (json.RawMessage, bool, error)) ([]byte, error) {
	o, err := parseObject(raw)
	if err != nil {
		return nil, err
	}
	kept := &object{vals: map[string]json.RawMessage{}}
	for _, k := range o.keys {
		val, ok, err := keep(k, o.vals[k])
		if err != nil {
			return nil, err
		}
		if ok {
			kept.set(k, val)
		}
	}
	return kept.marshal()
}

// CheckFile is the read-only audit: it reports why file would make Claude Code throw, nil when it is valid.
func CheckFile(file string) error {
	data, err := readCapped(file, maxSettingsBytes)
	if err != nil {
		return err
	}
	return Check(data)
}

// Check parses data as a JSON object and validates every key the schema documents; unknown keys pass.
func Check(data []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return located(data, err)
	}
	if top == nil {
		return errors.New("not a JSON object")
	}
	for _, k := range slices.Sorted(maps.Keys(top)) {
		if sk, ok := schemaKey(k); ok && !sk.Env {
			if err := sk.Validate(top[k]); err != nil {
				return err
			}
		}
	}
	return nil
}

// located prefixes a decode error with the line and column its byte offset falls on.
func located(data []byte, err error) error {
	var off int64
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		off = syn.Offset
	case errors.As(err, &typ):
		off = typ.Offset
	default:
		return err
	}
	before := data[:min(int(off), len(data))]
	line := bytes.Count(before, []byte("\n")) + 1
	col := len(before) - bytes.LastIndexByte(before, '\n')
	return fmt.Errorf("line %d col %d: %w", line, col, err)
}

//go:embed claude-code-settings.schema.json
var schemaJSON []byte

// Kind is the shape a schema key's value takes in settings.json.
type Kind int

const (
	kindBool Kind = iota
	kindEnum
	kindInt
	kindString
	kindObject
	kindNumber
	kindArray
)

// Key is one setting (or env variable) the schemastore schema documents.
type Key struct {
	Name       string
	Kind       Kind
	Enum       []string
	Min        *float64
	Max        *float64
	ExMin      *float64 // exclusive bounds
	ExMax      *float64
	Env        bool
	pattern    *regexp.Regexp // alternate accepted string form (theme "custom:<slug>")
	patternErr error          // a pattern that failed to compile: Validate refuses rather than skip the check
	alts       []Key          // union branches (anyOf, oneOf, a type list): valid when any accepts
	items      *Key           // array element schema
	props      map[string]Key // object property schemas
	required   []string
	constVal   json.RawMessage
	constWant  string // compactStr(constVal), computed once when the Key is built
	noExtra    bool   // additionalProperties: false
	addl       *Key   // additionalProperties schema applied to unlisted properties
	ref        bool   // uses $ref, which is not resolved: childRef and validateProperty read it
	untyped    bool   // no type, enum or not of its own (a bare $ref or {}): any value passes
	nullable   bool   // the type list names "null"
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
	Type       schemaType            `json:"type"`
	Enum       schemaEnum            `json:"enum"`
	Minimum    *float64              `json:"minimum"`
	Maximum    *float64              `json:"maximum"`
	ExMinimum  *float64              `json:"exclusiveMinimum"`
	ExMaximum  *float64              `json:"exclusiveMaximum"`
	Pattern    string                `json:"pattern"`
	AnyOf      []schemaNode          `json:"anyOf"`
	OneOf      []schemaNode          `json:"oneOf"`
	Items      *schemaNode           `json:"items"`
	Properties map[string]schemaNode `json:"properties"`
	Required   []string              `json:"required"`
	Const      json.RawMessage       `json:"const"`
	AddlProps  json.RawMessage       `json:"additionalProperties"`
	Ref        string                `json:"$ref"`
	Not        json.RawMessage       `json:"not"`
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
		ExMax: n.ExMaximum, Env: env}
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
	k.constWant, _ = compactStr(n.Const) // a schema constant is always valid JSON
	k.noExtra = strings.TrimSpace(string(n.AddlProps)) == "false"
	k.addl = n.addlKey(name, env)
	k.ref = n.Ref != "" || k.childRef()
	k.Kind = n.kind()
	k.untyped = n.Type.first() == "" && len(n.Enum) == 0 && len(n.Not) == 0
	k.nullable = slices.Contains(n.Type, "null")
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
		return kindEnum
	}
	switch n.Type.first() {
	case "boolean":
		return kindBool
	case "integer":
		return kindInt
	case "number":
		return kindNumber
	case "object":
		return kindObject
	case "array":
		return kindArray
	}
	return kindString
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
	for name, n := range doc.Properties["env"].Properties {
		if _, clash := m[name]; !clash {
			m[name] = n.key(name, true)
		}
	}
	return m
})

// schemaKey returns the schema entry for a settings key or an env variable name.
func schemaKey(name string) (Key, bool) {
	k, ok := schemaIndex()[name]
	return k, ok
}

// Validate checks raw against the key's schema: kind, enum, const, unions, pattern, bounds, required, extra props.
func (k Key) Validate(raw json.RawMessage) error {
	if k.patternErr != nil {
		return fmt.Errorf("%s: schema pattern does not compile: %w", k.Name, k.patternErr)
	}
	if strings.TrimSpace(string(raw)) == "null" {
		if k.nullable {
			return nil
		}
		return fmt.Errorf("%s: want a value, got null", k.Name)
	}
	if len(k.constVal) > 0 {
		a, errA := compactStr(raw)
		if errA != nil || a != k.constWant {
			return fmt.Errorf("%s: must be %s", k.Name, k.constVal)
		}
	}
	if len(k.alts) > 0 {
		return k.validateAlternatives(raw)
	}
	if k.untyped {
		return nil
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
	case kindBool:
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return fmt.Errorf("%s: want a boolean", k.Name)
		}
	case kindInt, kindNumber:
		return k.validateNumber(raw)
	case kindEnum:
		return k.validateEnum(raw)
	case kindString:
		return k.validateString(raw)
	case kindObject:
		return k.validateObject(raw)
	case kindArray:
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
	if k.Kind == kindInt && n != float64(int64(n)) {
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
	sub, ok := k.props[pn]
	if !ok && k.noExtra {
		return fmt.Errorf("%s: unknown property %q", k.Name, pn)
	}
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

// compactStr compacts raw JSON for comparison; a missing value (nil) compacts to "".
func compactStr(raw json.RawMessage) (string, error) {
	if raw == nil {
		return "", nil
	}
	// Decode and re-encode so escape-equivalent spellings (\u003c vs <) compare equal.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	raw, err := textutil.MarshalNoEscape(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// SettingsFile is the live Claude Code settings.json under the config dir.
func SettingsFile() string { return filepath.Join(paths.ConfigDir(), "settings.json") }

// PermissionLists is the part of a settings.json `permissions` block the guards read.
type PermissionLists struct {
	Allow []string `json:"allow"`
	Ask   []string `json:"ask"`
	Deny  []string `json:"deny"`
}

// LivePermissions reads the live settings.json's permissions lists; a missing or invalid file is an error.
func LivePermissions() (PermissionLists, error) {
	var doc struct {
		Permissions *struct {
			Allow []string  `json:"allow"`
			Ask   []string  `json:"ask"`
			Deny  *[]string `json:"deny"`
		} `json:"permissions"`
	}
	data, err := readCapped(SettingsFile(), maxSettingsBytes)
	if err != nil {
		return PermissionLists{}, err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return PermissionLists{}, fmt.Errorf("settings.json: %w", err)
	}
	// An absent or null deny list must fall back to the template, not protect nothing; [] is an explicit choice.
	if doc.Permissions == nil || doc.Permissions.Deny == nil {
		return PermissionLists{}, errors.New("settings.json: permissions.deny is missing")
	}
	p := doc.Permissions
	return PermissionLists{Allow: p.Allow, Ask: p.Ask, Deny: *p.Deny}, nil
}

const maxSettingsBytes = 4 << 20

// ErrTooLarge marks a settings file over the read cap.
var ErrTooLarge = errors.New("settings.json over 4 MiB")

// readCapped reads file, failing with ErrTooLarge when it holds more than max bytes.
func readCapped(file string, max int64) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%w (%s): trim it below 4 MiB or safety-scan keeps using template permissions", ErrTooLarge, filepath.Base(file))
	}
	return data, nil
}

// SeedSettings writes Render (via transform when non-nil) only when file is absent; an existing file is untouched.
func SeedSettings(file string, transform func([]byte) ([]byte, error)) (seeded bool, err error) {
	err = paths.WithFileLock(file, func() error {
		if _, serr := os.Stat(file); !errors.Is(serr, fs.ErrNotExist) {
			return serr
		}
		data, err := Render()
		if err == nil && transform != nil {
			data, err = transform(data)
		}
		if err != nil {
			return err
		}
		seeded = true
		return paths.AtomicWrite(file, data, 0o644)
	})
	return seeded && err == nil, err
}

// ResetSettings is the owner-run `configure --restore`: copy file to file.bak.<timestamp>, then write Render whole.
func ResetSettings(file string) (backup string, err error) {
	err = paths.WithFileLock(file, func() error {
		mode := os.FileMode(0o644)
		old, rerr := os.ReadFile(file)
		switch {
		case rerr == nil:
			if fi, serr := os.Stat(file); serr == nil {
				mode = fi.Mode().Perm()
			}
			backup = file + ".bak." + time.Now().Format("20060102-150405.000000000")
			if err := writeBackup(backup, old, mode); err != nil {
				return fmt.Errorf("backup %s: %w", file, err)
			}
		case !errors.Is(rerr, fs.ErrNotExist):
			return fmt.Errorf("read %s: %w", file, rerr)
		}
		data, err := Render()
		if err != nil {
			return err
		}
		return paths.AtomicWrite(file, data, mode)
	})
	return backup, err
}

// writeBackup creates the backup exclusively so a same-instant restore never truncates an earlier one.
func writeBackup(name string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Normalize collapses duplicate keys, string-list entries and hook handlers in data, then applies Format's order.
func Normalize(data []byte) ([]byte, []string, error) {
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, located(data, err)
	}
	var changed []string
	collapsed, err := collapseValue(raw, "", &changed)
	if err != nil {
		return nil, nil, err
	}
	out, err := Format(collapsed)
	return out, changed, err
}

func collapseValue(raw json.RawMessage, path string, changed *[]string) (json.RawMessage, error) {
	switch bytes.TrimSpace(raw)[0] {
	case '{':
		return collapseObject(raw, path, changed)
	case '[':
		return collapseArray(raw, path, changed)
	}
	return raw, nil
}

func collapseObject(raw json.RawMessage, path string, changed *[]string) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	o := &object{vals: map[string]json.RawMessage{}}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		if _, dup := o.vals[k]; dup {
			*changed = append(*changed, "duplicate key "+join(path, k))
		}
		o.set(k, val)
	}
	for _, k := range o.keys {
		v, err := collapseValue(o.vals[k], join(path, k), changed)
		if err != nil {
			return nil, err
		}
		o.vals[k] = v
	}
	return o.marshal()
}

func collapseArray(raw json.RawMessage, path string, changed *[]string) (json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	hookList := strings.HasPrefix(path, "hooks.") && strings.Count(path, ".") == 1
	seen := map[string]bool{}
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if bytes.TrimSpace(item)[0] == '"' {
			if seen[string(item)] {
				*changed = append(*changed, "duplicate entry "+path+" "+string(item))
				continue
			}
			seen[string(item)] = true
		}
		v, err := collapseValue(item, path+"[]", changed)
		if err != nil {
			return nil, err
		}
		if hookList {
			var keep bool
			if v, keep = dedupHookEntry(v, seen, path, changed); !keep {
				continue
			}
		}
		out = append(out, v)
	}
	return textutil.MarshalNoEscape(out)
}

// dedupHookEntry drops handlers identical (all options) to one seen under the same matcher; keep is false when none remain.
func dedupHookEntry(entry json.RawMessage, seen map[string]bool, path string, changed *[]string) (json.RawMessage, bool) {
	var e map[string]json.RawMessage
	var hs []map[string]json.RawMessage
	if json.Unmarshal(entry, &e) != nil || json.Unmarshal(e["hooks"], &hs) != nil || len(hs) == 0 {
		return entry, true
	}
	kept := hs[:0:0]
	for _, h := range hs {
		raw, _ := textutil.MarshalNoEscape(h) // map keys marshal sorted, so equal handlers give equal bytes
		id := string(e["matcher"]) + "\x00" + string(raw)
		if string(h["command"]) != "" && seen[id] {
			*changed = append(*changed, "duplicate hook "+path+" "+string(h["command"]))
			continue
		}
		seen[id] = true
		kept = append(kept, h)
	}
	if len(kept) == len(hs) {
		return entry, true
	}
	if len(kept) == 0 {
		return nil, false
	}
	e["hooks"], _ = textutil.MarshalNoEscape(kept)
	out, _ := textutil.MarshalNoEscape(e)
	return out, true
}

func join(path, k string) string { return strings.TrimPrefix(path+"."+k, ".") }
