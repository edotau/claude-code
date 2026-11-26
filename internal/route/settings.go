package route

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
)

// Allowed values for the enum fields of a record and an overlay.
const (
	FallbackNone   = "none"
	FallbackPin    = "pin"
	FallbackAgents = "agents"
	FallbackAll    = "all"

	RunnerPi  = "pi"
	RunnerCLI = "cli"

	CodemodeOff  = "off"
	CodemodeOn   = "on"
	CodemodeOnly = "only"

	VerbosityLow    = "low"
	VerbosityMedium = "medium"
	VerbosityHigh   = "high"

	// ConfigRouteSession is the config_route value meaning the session's own route.
	ConfigRouteSession = "session"
)

// FallbackModes, Runners, Codemodes and Verbosities list the allowed values of their fields.
var (
	FallbackModes = []string{FallbackNone, FallbackPin, FallbackAgents, FallbackAll}
	Runners       = []string{RunnerPi, RunnerCLI}
	Codemodes     = []string{CodemodeOff, CodemodeOn, CodemodeOnly}
	Verbosities   = []string{VerbosityLow, VerbosityMedium, VerbosityHigh}
)

// OneOf refuses v outside allowed, naming field; an empty v is valid.
func OneOf(field, v string, allowed []string) error {
	if v != "" && !slices.Contains(allowed, v) {
		return fmt.Errorf("%s: %q is not one of %s", field, v, strings.Join(allowed, "|"))
	}
	return nil
}

// UnknownKey is the one refusal of a settings key outside valid: it names the nearest valid key, else lists them.
func UnknownKey(key string, valid []string) error {
	near, ok := textutil.Nearest(key, valid)
	if !ok {
		near = "" // Nearest returns its best guess even when it is too far
	}
	for _, v := range valid { // fallback_mode → fallback: a shared stem of 4+ runes when no near spelling exists
		if n := len(commonPrefix(key, v)); !ok && n >= 4 && n > len(commonPrefix(key, near)) {
			near = v
		}
	}
	if near == "" {
		return fmt.Errorf("unknown key %q (valid keys: %s)", key, strings.Join(valid, "|"))
	}
	return fmt.Errorf("unknown key %q (nearest valid key: %s)", key, near)
}

func commonPrefix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}

// CheckKeys refuses the first key of the JSON object raw that shape's json tags lack, nested objects included.
func CheckKeys(raw []byte, shape reflect.Type) error {
	for shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	custom := reflect.PointerTo(shape).Implements(reflect.TypeFor[json.Unmarshaler]())
	keys, vals, isObj := objectMembers(raw)
	if custom || !isObj || (shape.Kind() != reflect.Struct && shape.Kind() != reflect.Map) {
		return nil // not a plain object: the type's own decode reports it
	}
	for i, k := range keys {
		t, err := memberType(shape, k)
		if err != nil {
			return err
		}
		if err := CheckKeys(vals[i], t); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

// memberType is the type key k decodes into: a map's element, or the struct field encoding/json matches k to.
func memberType(shape reflect.Type, k string) (reflect.Type, error) {
	if shape.Kind() == reflect.Map {
		return shape.Elem(), nil
	}
	fields := jsonFields(shape)
	if t, ok := fields[k]; ok {
		return t, nil
	}
	names := slices.Sorted(maps.Keys(fields))
	if j := slices.IndexFunc(names, func(n string) bool { return strings.EqualFold(n, k) }); j >= 0 {
		return fields[names[j]], nil
	}
	return nil, UnknownKey(k, names)
}

// objectMembers is every member of the JSON object raw in document order, duplicate keys kept; false for a non-object.
func objectMembers(raw []byte) (keys []string, vals []json.RawMessage, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil, false
	}
	for dec.More() {
		tok, err := dec.Token()
		k, isKey := tok.(string)
		var v json.RawMessage
		if err != nil || !isKey || dec.Decode(&v) != nil {
			return nil, nil, false
		}
		keys, vals = append(keys, k), append(vals, v)
	}
	return keys, vals, true
}

// jsonFields maps each json key of struct type t to its field type, as encoding/json names and flattens them.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case !f.IsExported() || name == "-":
		case name == "" && f.Anonymous && f.Type.Kind() == reflect.Struct:
			maps.Copy(fields, jsonFields(f.Type))
		case name == "":
			fields[f.Name] = f.Type
		default:
			fields[name] = f.Type
		}
	}
	return fields
}
