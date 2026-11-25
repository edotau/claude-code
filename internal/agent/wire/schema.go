package wire

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// errSchema marks an answer that is not JSON satisfying the requested schema; the envelope kind is "schema".
var errSchema = errors.New("schema")

// SchemaInstruction is the one line that asks an agent without a native flag for schema-shaped JSON.
func SchemaInstruction(schema string) string {
	return assets.AgentSchemaInstruction(compactJSON(schema))
}

func compactJSON(s string) string {
	var b bytes.Buffer
	if json.Compact(&b, []byte(s)) != nil {
		return s
	}
	return b.String()
}

// Structured parses the answer as JSON and checks it against schema (stdlib-only: type, required, properties, items).
func Structured(answer, schema string) (raw json.RawMessage, unchecked []string, err error) {
	span := jsonSpan(answer)
	v, err := decodeExact(span)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: answer is not JSON: %v", errSchema, err)
	}
	sch, err := decodeExact(schema)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: unreadable schema: %v", errSchema, err)
	}
	if err = schemaShape(sch); err != nil {
		return nil, nil, fmt.Errorf("%w: malformed schema: %v", errSchema, err)
	}
	skipped := map[string]bool{}
	collectUnchecked(sch, skipped)
	var dropped []string
	if v = pruneExtras("answer", v, sch, &dropped); len(dropped) > 0 {
		for _, d := range dropped {
			skipped["dropped "+d] = true
		}
		if span, err = encodeExact(v); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", errSchema, err)
		}
	}
	if err = checkSchema("answer", v, sch); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errSchema, err)
	}
	return json.RawMessage(span), slices.Sorted(maps.Keys(skipped)), nil
}

var decodeSchemas sync.Map // reflect.Type -> schema string

// Decode checks answer against SchemaFor[T]() via Structured and unmarshals the checked document into T.
func Decode[T any](answer string) (T, []string, error) {
	var v T
	t := reflect.TypeFor[T]()
	s, ok := decodeSchemas.Load(t)
	if !ok {
		s, _ = decodeSchemas.LoadOrStore(t, SchemaFor[T]())
	}
	raw, unchecked, err := Structured(answer, s.(string))
	if err != nil {
		return v, nil, err
	}
	return v, unchecked, json.Unmarshal(raw, &v)
}

func encodeExact(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// optionalNull reports a null value on a declared property that is not required; it counts as absent.
func optionalNull(m, props map[string]any, k string, val any) bool {
	req, _ := m["required"].([]any)
	return val == nil && props[k] != nil && !slices.Contains(req, any(k))
}

// pruneExtras returns v without the keys an additionalProperties:false object does not declare; dropped names them.
func pruneExtras(path string, v, sch any, dropped *[]string) any {
	m, _ := sch.(map[string]any)
	switch x := v.(type) {
	case map[string]any:
		props, _ := m["properties"].(map[string]any)
		out := make(map[string]any, len(x))
		for k, val := range x {
			if optionalNull(m, props, k, val) {
				*dropped = append(*dropped, path+"."+k+" (null)")
				continue
			}
			if _, declared := props[k]; !declared && m["additionalProperties"] == false {
				*dropped = append(*dropped, path+"."+k)
				continue
			}
			out[k] = pruneExtras(path+"."+k, val, cmp.Or(props[k], m["additionalProperties"]), dropped)
		}
		return out
	case []any:
		item, isSchema := m["items"].(map[string]any)
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
			if isSchema {
				out[i] = pruneExtras(path+"[]", e, item, dropped)
			}
		}
		return out
	}
	return v
}

// decodeExact decodes one JSON value with numbers kept as json.Number, so no integer loses precision.
func decodeExact(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

// jsonEqual is enum equality: numbers compare by exact value (1 == 1.0), everything else structurally.
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		rx, okx := new(big.Rat).SetString(string(x))
		ry, oky := new(big.Rat).SetString(string(y))
		return okx && oky && rx.Cmp(ry) == 0
	case []any:
		y, ok := b.([]any)
		return ok && slices.EqualFunc(x, y, jsonEqual)
	case map[string]any:
		y, ok := b.(map[string]any)
		return ok && maps.EqualFunc(x, y, jsonEqual)
	}
	return a == b
}

// schemaKnown are the keywords checkSchema enforces; schemaNotes only annotate. Every other keyword is unchecked.
var (
	schemaKnown = map[string]bool{"type": true, "required": true, "properties": true, "items": true, "additionalItems": true, "enum": true,
		"additionalProperties": true}
	schemaNotes = map[string]bool{"$schema": true, "$id": true, "title": true, "description": true, "default": true, "examples": true,
		"$comment": true, "$defs": true, "definitions": true}
)

func checkSchema(path string, v, sch any) error {
	if b, ok := sch.(bool); ok && !b {
		return fmt.Errorf("%s is forbidden by the schema (false)", path)
	}
	m, ok := sch.(map[string]any)
	if !ok {
		return nil // a true or absent schema allows any value
	}
	if err := checkTypeEnum(path, v, m); err != nil {
		return err
	}
	switch x := v.(type) {
	case map[string]any:
		return checkSchemaObject(path, x, m)
	case []any:
		return checkSchemaArray(path, x, m)
	}
	return nil
}

// checkTypeEnum enforces a schema's type and enum keywords on v.
func checkTypeEnum(path string, v any, m map[string]any) error {
	if t, ok := m["type"]; ok && !typeMatches(v, t) {
		return fmt.Errorf("%s is %s, schema wants %v", path, jsonTypeName(v), t)
	}
	if enum, ok := m["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return jsonEqual(e, v) }) {
		return fmt.Errorf("%s is not one of the schema's enum values", path)
	}
	return nil
}

// checkSchemaArray checks each element against items, or against its position when items is a tuple (draft-07).
func checkSchemaArray(path string, values []any, m map[string]any) error {
	items := m["items"]
	tuple, isTuple := items.([]any)
	for i, el := range values {
		sch := items
		if isTuple { // past the tuple, additionalItems governs; absent means any
			sch = m["additionalItems"]
			if i < len(tuple) {
				sch = tuple[i]
			}
		}
		if err := checkSchema(fmt.Sprintf("%s[%d]", path, i), el, sch); err != nil {
			return err
		}
	}
	return nil
}

// checkSchemaObject enforces required, then each declared property in key order.
func checkSchemaObject(path string, x, m map[string]any) error {
	if req, ok := m["required"].([]any); ok {
		for _, k := range req {
			if name, isStr := k.(string); isStr { // "" is a valid property name, so it is checked too
				if _, ok := x[name]; !ok {
					return fmt.Errorf("%s: missing required key %q", path, name)
				}
			}
		}
	}
	props, _ := m["properties"].(map[string]any)
	for _, k := range slices.Sorted(maps.Keys(props)) {
		if val, ok := x[k]; ok {
			if err := checkSchema(path+"."+k, val, props[k]); err != nil {
				return err
			}
		}
	}
	return checkExtras(path, x, props, m)
}

// checkExtras applies additionalProperties to the keys not declared in props: false forbids them, a schema checks each.
func checkExtras(path string, x, props, m map[string]any) error {
	ap, has := m["additionalProperties"]
	if !has {
		return nil
	}
	for _, k := range slices.Sorted(maps.Keys(x)) {
		if _, declared := props[k]; declared {
			continue
		}
		if err := checkSchema(path+"."+k, x[k], ap); err != nil {
			return fmt.Errorf("%s: unexpected key %q: %w", path, k, err)
		}
	}
	return nil
}

// schemaShape rejects a malformed enforced keyword the validator would skip; it recurses into subschemas.
func schemaShape(sch any) error {
	m, ok := sch.(map[string]any)
	if !ok {
		return nil
	}
	if err := keywordShape(m); err != nil {
		return err
	}
	if err := propertiesShape(m); err != nil {
		return err
	}
	if tuple, ok := m["items"].([]any); ok {
		for _, s := range tuple {
			if err := schemaShape(s); err != nil {
				return err
			}
		}
	} else if err := schemaShape(m["items"]); err != nil {
		return err
	}
	if err := schemaShape(m["additionalProperties"]); err != nil {
		return err
	}
	return schemaShape(m["additionalItems"])
}

// propertiesShape rejects a non-object properties keyword and recurses into each property schema.
func propertiesShape(m map[string]any) error {
	props, has := m["properties"]
	if !has {
		return nil
	}
	pm, ok := props.(map[string]any)
	if !ok {
		return errors.New("properties must be an object")
	}
	for _, p := range pm {
		if err := schemaShape(p); err != nil {
			return err
		}
	}
	return nil
}

// keywordShape rejects a malformed required or enum keyword.
func keywordShape(m map[string]any) error {
	if req, has := m["required"]; has {
		names, ok := req.([]any)
		if !ok {
			return errors.New("required must be an array of property names")
		}
		for _, n := range names {
			if _, isStr := n.(string); !isStr {
				return errors.New("required must hold only property names")
			}
		}
	}
	if enum, has := m["enum"]; has {
		if _, ok := enum.([]any); !ok {
			return errors.New("enum must be an array")
		}
	}
	return nil
}

// collectUnchecked adds every keyword in sch (and its properties and items) that checkSchema does not enforce.
func collectUnchecked(sch any, skipped map[string]bool) {
	m, ok := sch.(map[string]any)
	if !ok {
		return
	}
	for k := range m {
		if !schemaKnown[k] && !schemaNotes[k] {
			skipped[k] = true
		}
	}
	props, _ := m["properties"].(map[string]any)
	for _, p := range props {
		collectUnchecked(p, skipped)
	}
	if tuple, ok := m["items"].([]any); ok { // tuple form: every positional schema
		for _, s := range tuple {
			collectUnchecked(s, skipped)
		}
	} else {
		collectUnchecked(m["items"], skipped)
	}
	collectUnchecked(m["additionalItems"], skipped)
	collectUnchecked(m["additionalProperties"], skipped)
}

// typeMatches checks val against a schema "type" given as a name or a list of names.
func typeMatches(val, t any) bool {
	switch tt := t.(type) {
	case string:
		return jsonTypeIs(val, tt)
	case []any:
		for _, n := range tt {
			if s, ok := n.(string); ok && jsonTypeIs(val, s) {
				return true
			}
		}
	}
	return false
}

func jsonTypeName(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		if r, ok := new(big.Rat).SetString(string(x)); ok && r.IsInt() {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	}
	return "object"
}

func jsonTypeIs(v any, t string) bool {
	n := jsonTypeName(v)
	return n == t || (t == "number" && n == "integer")
}

// spanPairs orders the {..} and [..] delimiters so the one whose opener comes first (the outer span) is tried first.
func spanPairs(s string) [][2]string {
	if o, a := strings.Index(s, "{"), strings.Index(s, "["); a >= 0 && (o < 0 || a < o) {
		return [][2]string{{"[", "]"}, {"{", "}"}}
	}
	return [][2]string{{"{", "}"}, {"[", "]"}}
}

// jsonSpan is the JSON in an answer: the text itself, a fenced block, or the outermost {...} / [...] span.
func jsonSpan(answer string) string {
	s := strings.TrimSpace(answer)
	if json.Valid([]byte(s)) {
		return s
	}
	if _, after, ok := strings.Cut(s, "```"); ok {
		body := after
		body = body[strings.IndexByte(body, '\n')+1:]
		if j := strings.Index(body, "```"); j >= 0 && json.Valid([]byte(strings.TrimSpace(body[:j]))) {
			return strings.TrimSpace(body[:j])
		}
	}
	for _, p := range spanPairs(s) {
		if i, j := strings.Index(s, p[0]), strings.LastIndex(s, p[1]); i >= 0 && j > i && json.Valid([]byte(s[i:j+1])) {
			return s[i : j+1]
		}
	}
	return s
}

var (
	_ = schemas.Register(schemas.Format{
		Name: "dispatch-events", Owner: "internal/agent/wire", Kind: schemas.Log, Version: EventSchema,
		Glob:  func() string { return filepath.Join(paths.DispatchDir(), "*"+paths.DispatchEventsSuffix) },
		New:   func() any { return new(Event) },
		Clean: "dispatch-log",
	})

	_ = schemas.Register(schemas.Format{
		Name: "ask-envelope", Owner: "internal/agent/wire", Kind: schemas.Wire, Version: EnvelopeSchema,
		New: func() any { return new(Envelope) },
	})

	_ = schemas.Register(schemas.Format{
		Name: "live-request", Owner: "internal/agent/wire", Kind: schemas.Wire, Version: LiveSchema,
		New: func() any { return new(LiveRequest) },
	})
)

// SchemaFor is the JSON Schema of T; API shape after github.com/zendev-sh/goai SchemaFrom (MIT), code written here.
func SchemaFor[T any]() string {
	b, err := json.Marshal(schemaOf(reflect.TypeFor[T](), map[reflect.Type]bool{}))
	if err != nil {
		panic("wire.SchemaFor: " + err.Error()) // only plain maps and strings are marshaled
	}
	return string(b)
}

var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

// schemaOf panics on a recursive type: callers are package-level vars, so it surfaces at init.
func schemaOf(t reflect.Type, path map[reflect.Type]bool) map[string]any {
	if t.Name() != "" {
		if path[t] {
			panic("wire.SchemaFor: recursive type " + t.String())
		}
		path[t] = true
		defer delete(path, t)
	}
	if sc := scalarSchema(t); sc != nil {
		return sc
	}
	switch t.Kind() {
	case reflect.Pointer:
		return schemaOf(t.Elem(), path)
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaOf(t.Elem(), path)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaOf(t.Elem(), path)}
	case reflect.Struct:
		props, required := map[string]any{}, []string{}
		structFields(t, path, props, &required)
		return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": props}
	}
	return map[string]any{}
}

// scalarSchema is the schema of a leaf type (time, bytes, raw JSON included), or nil for a composite.
func scalarSchema(t reflect.Type) map[string]any {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == rawType:
		return map[string]any{}
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8:
		return map[string]any{"type": "string"}
	}
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	}
	return nil
}

// structFields follows encoding/json: an untagged embedded struct flattens, a tagged one nests, "-" drops.
func structFields(t reflect.Type, path map[reflect.Type]bool, props map[string]any, required *[]string) {
	for _, f := range reflect.VisibleFields(t) {
		if len(f.Index) > 1 { // promoted fields come through their embedded struct below
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case f.Tag.Get("json") == "-":
		case f.Anonymous && name == "" && ft.Kind() == reflect.Struct:
			structFields(ft, path, props, required)
		case f.IsExported():
			props[cmp.Or(name, f.Name)] = fieldSchema(f, path)
			if !strings.Contains(","+opts+",", ",omitempty,") {
				*required = append(*required, cmp.Or(name, f.Name))
			}
		}
	}
}

func fieldSchema(f reflect.StructField, path map[reflect.Type]bool) map[string]any {
	p := schemaOf(f.Type, path)
	for opt := range strings.SplitSeq(f.Tag.Get("jsonschema"), ",") {
		if vals, ok := strings.CutPrefix(opt, "enum="); ok {
			p["enum"] = strings.Split(vals, "|")
		} else if desc, ok := strings.CutPrefix(opt, "description="); ok {
			p["description"] = desc
		}
	}
	return p
}
