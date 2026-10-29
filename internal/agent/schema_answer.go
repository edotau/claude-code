package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// structuredAnswer parses the answer as JSON and checks it against schema; the validator is minimal:
// top-level type, required keys and the declared type of each top-level property (stdlib only).
func structuredAnswer(answer, schema string) (json.RawMessage, error) {
	raw := jsonSpan(answer)
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("%w: answer is not JSON: %v", ErrSchema, err)
	}
	var s struct {
		Type       string                        `json:"type"`
		Required   []string                      `json:"required"`
		Properties map[string]struct{ Type any } `json:"properties"`
	}
	if err := json.Unmarshal([]byte(schema), &s); err != nil {
		return nil, fmt.Errorf("%w: unreadable schema: %v", ErrSchema, err)
	}
	if s.Type != "" && !jsonTypeIs(v, s.Type) {
		return nil, fmt.Errorf("%w: answer is %s, schema wants %s", ErrSchema, jsonTypeName(v), s.Type)
	}
	obj, isObj := v.(map[string]any)
	if !isObj && (len(s.Required) > 0 || len(s.Properties) > 0) {
		return nil, fmt.Errorf("%w: answer is %s, schema wants an object", ErrSchema, jsonTypeName(v))
	}
	for _, k := range s.Required {
		if _, ok := obj[k]; !ok {
			return nil, fmt.Errorf("%w: missing required key %q", ErrSchema, k)
		}
	}
	for k, p := range s.Properties {
		val, ok := obj[k]
		if !ok || p.Type == nil {
			continue
		}
		if !typeMatches(val, p.Type) {
			return nil, fmt.Errorf("%w: key %q is %s, schema wants %v", ErrSchema, k, jsonTypeName(val), p.Type)
		}
	}
	return json.RawMessage(raw), nil
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
	case float64:
		if x == float64(int64(x)) {
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
	if i := strings.Index(s, "```"); i >= 0 {
		body := s[i+3:]
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

// ValidEnvName reports why name cannot be set with --env: credential-shaped names belong in the vault.
func ValidEnvName(name string) error {
	if name == "" || strings.ContainsAny(name, "= \t") {
		return fmt.Errorf("--env %q: want NAME=VALUE", name)
	}
	up := strings.ToUpper(name)
	if strings.HasSuffix(up, "_API_KEY") || strings.HasSuffix(up, "_TOKEN") || strings.HasSuffix(up, "_SECRET") || strings.Contains(up, "AUTH") {
		return fmt.Errorf("--env %s looks like a credential: store it with `claude-code vault set` or a provider's auth.command, not on the command line", name)
	}
	return nil
}

// ValidateEnv checks every KEY=VALUE entry of --env.
func ValidateEnv(env []string) error {
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("--env %q: want NAME=VALUE", kv)
		}
		if err := ValidEnvName(name); err != nil {
			return err
		}
	}
	return nil
}
