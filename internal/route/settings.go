package route

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/textutil"
)

// Allowed values for the enum fields of AgentSettings.
const (
	FallbackNone   = "none"
	FallbackPin    = "pin"
	FallbackAgents = "agents"
	FallbackAll    = "all"

	RunnerPi  = "pi"
	RunnerOwn = "own"

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
	Runners       = []string{RunnerPi, RunnerOwn}
	Codemodes     = []string{CodemodeOff, CodemodeOn, CodemodeOnly}
	Verbosities   = []string{VerbosityLow, VerbosityMedium, VerbosityHigh}
)

// Budget caps one run; zero means unset.
type Budget struct {
	USD           float64 `json:"usd,omitempty"`
	Credits       float64 `json:"credits,omitempty"`
	MaxChildren   int     `json:"max_children,omitempty"`
	MaxTreeTokens int     `json:"max_tree_tokens,omitempty"`
}

// ConfigRoute is either the session's route (Session) or an explicit Route.
type ConfigRoute struct {
	Session bool
	Route   Route
}

// MarshalJSON writes the string "session" or the route object.
func (c ConfigRoute) MarshalJSON() ([]byte, error) {
	if c.Session {
		return json.Marshal(ConfigRouteSession)
	}
	return json.Marshal(c.Route)
}

// UnmarshalJSON reads the string "session" or a route object.
func (c *ConfigRoute) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		if s != ConfigRouteSession {
			return fmt.Errorf("config_route: %q is not %q or a route", s, ConfigRouteSession)
		}
		*c = ConfigRoute{Session: true}
		return nil
	}
	var r Route
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("config_route: %w", err)
	}
	*c = ConfigRoute{Route: r}
	return nil
}

// AgentSettings is the one shape of an agent's settings, shared by every surface that spells them.
type AgentSettings struct {
	Route         *Route            `json:"route,omitempty"`
	FallbackRoute *Route            `json:"fallback_route,omitempty"`
	FallbackMode  string            `json:"fallback_mode,omitempty"`
	Model         string            `json:"model,omitempty"`
	Effort        string            `json:"effort,omitempty"`
	Runner        string            `json:"runner,omitempty"`
	Codemode      string            `json:"codemode,omitempty"`
	Tools         *bool             `json:"tools,omitempty"`
	Timeout       string            `json:"timeout,omitempty"`
	Verbosity     string            `json:"verbosity,omitempty"`
	ConfigRoute   *ConfigRoute      `json:"config_route,omitempty"`
	Context       string            `json:"context,omitempty"`
	Budget        *Budget           `json:"budget,omitempty"`
	Extras        map[string]string `json:"extras,omitempty"`
}

// Validate reports the first field outside its allowed values; empty fields are valid.
func (s AgentSettings) Validate() error {
	for _, c := range []struct {
		field, v string
		allowed  []string
	}{
		{"fallback_mode", s.FallbackMode, FallbackModes},
		{"effort", s.Effort, agentid.EffortNames()},
		{"runner", s.Runner, Runners},
		{"codemode", s.Codemode, Codemodes},
		{"verbosity", s.Verbosity, Verbosities},
	} {
		if c.v != "" && !slices.Contains(c.allowed, c.v) {
			return fmt.Errorf("%s: %q is not one of %s", c.field, c.v, strings.Join(c.allowed, "|"))
		}
	}
	return s.validateNumbers()
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
	names := slices.Sorted(maps.Keys(fields))
	if t, ok := fields[k]; ok {
		return t, nil
	}
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

func (s AgentSettings) validateNumbers() error {
	if s.Timeout != "" {
		if _, err := time.ParseDuration(s.Timeout); err != nil {
			return fmt.Errorf("timeout: %q is not a duration like 90s or 10m", s.Timeout)
		}
	}
	if b := s.Budget; b != nil && (b.USD < 0 || b.Credits < 0 || b.MaxChildren < 0 || b.MaxTreeTokens < 0) {
		return errors.New("budget: usd, credits, max_children and max_tree_tokens must be >= 0")
	}
	return nil
}
