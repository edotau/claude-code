package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"slices"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

// ErrSchema marks an answer that is not JSON satisfying the requested schema; the envelope kind is "schema".
var ErrSchema = errors.New("schema")

// schemaInstruction is the one line that asks an agent without a native flag for schema-shaped JSON.
func schemaInstruction(schema string) string {
	return "\n\nAnswer with only a JSON value matching this JSON Schema, no prose or code fence: " + compactJSON(schema)
}

func compactJSON(s string) string {
	var b bytes.Buffer
	if json.Compact(&b, []byte(s)) != nil {
		return s
	}
	return b.String()
}

// structuredAnswer parses the answer as JSON and checks it against schema. The validator is minimal and stdlib-only:
// type, required, properties, items and enum, recursively; any other validation keyword is returned in unchecked.
func structuredAnswer(answer, schema string) (raw json.RawMessage, unchecked []string, err error) {
	span := jsonSpan(answer)
	v, err := decodeExact(span)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: answer is not JSON: %v", ErrSchema, err)
	}
	sch, err := decodeExact(schema)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: unreadable schema: %v", ErrSchema, err)
	}
	skipped := map[string]bool{}
	collectUnchecked(sch, skipped)
	if err = checkSchema("answer", v, sch); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrSchema, err)
	}
	for k := range skipped {
		unchecked = append(unchecked, k)
	}
	sort.Strings(unchecked)
	return json.RawMessage(span), unchecked, nil
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
	schemaKnown = map[string]bool{"type": true, "required": true, "properties": true, "items": true, "enum": true}
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
	if t, ok := m["type"]; ok && !typeMatches(v, t) {
		return fmt.Errorf("%s is %s, schema wants %v", path, jsonTypeName(v), t)
	}
	if enum, ok := m["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return jsonEqual(e, v) }) {
		return fmt.Errorf("%s is not one of the schema's enum values", path)
	}
	switch x := v.(type) {
	case map[string]any:
		if req, ok := m["required"].([]any); ok {
			for _, k := range req {
				if name, _ := k.(string); name != "" {
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
	case []any:
		for i, el := range x {
			if err := checkSchema(fmt.Sprintf("%s[%d]", path, i), el, m["items"]); err != nil {
				return err
			}
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
	collectUnchecked(m["items"], skipped)
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
	for _, p := range [][2]string{{"{", "}"}, {"[", "]"}} {
		if i, j := strings.Index(s, p[0]), strings.LastIndex(s, p[1]); i >= 0 && j > i && json.Valid([]byte(s[i:j+1])) {
			return s[i : j+1]
		}
	}
	return s
}

// ValidEnvName reports why name cannot be set with --env: credential-shaped and harness-scrubbed names are refused.
// The error names NAME only; a value never appears in it.
func ValidEnvName(name string) error {
	if name == "" || strings.ContainsAny(name, " \t") {
		return errors.New("--env: want NAME=VALUE")
	}
	up := strings.ToUpper(name)
	reserved := proc.EnvBlocked(name) || proc.EnvBlocked(up)
	if strings.HasSuffix(up, "_BASE_URL") && !strings.HasPrefix(up, "HARNESS_") {
		reserved = false // a route override such as ANTHROPIC_BASE_URL is not a secret
	}
	switch {
	case reserved:
		return fmt.Errorf("--env %s is reserved by the harness credential scrub: use `claude-code vault set` or a provider's auth.command", name)
	case strings.HasSuffix(up, "_API_KEY"), strings.HasSuffix(up, "_TOKEN"), strings.HasSuffix(up, "_SECRET"), strings.Contains(up, "AUTH"):
		return fmt.Errorf("--env %s looks like a credential: store it with `claude-code vault set` or a provider's auth.command, not on the command line", name)
	}
	return nil
}

// ValidateEnv checks every KEY=VALUE entry of --env; errors never echo a value.
func ValidateEnv(env []string) error {
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			return errors.New("--env: want NAME=VALUE")
		}
		if err := ValidEnvName(name); err != nil {
			return err
		}
	}
	return nil
}
