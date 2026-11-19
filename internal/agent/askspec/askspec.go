// Package askspec is ask's flag schema, generated from its FlagSet and registered by ask at init.
package askspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

// Field is one ask flag as a spec key.
type Field struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"` // bool|int|duration|string
	List    bool     `json:"list,omitempty"`
	JSON    bool     `json:"json,omitempty"` // also takes an inline JSON object
	Choices []string `json:"choices,omitempty"`
	Default string   `json:"default,omitempty"`
	Usage   string   `json:"usage"`
}

// Schema is ask's flags in FlagSet (sorted) order.
type Schema struct{ Fields []Field }

// Opts adds what a flag.Value cannot show: fixed value sets and which flags take inline JSON.
type Opts struct {
	Choices map[string][]string
	JSON    []string
}

// PromptKey is the spec and batch key for the task text.
const PromptKey = "prompt"

// FromFlagSet builds the schema from fs, skipping internal flags and --spec itself.
func FromFlagSet(fs *flag.FlagSet, o Opts) Schema {
	var s Schema
	fs.VisitAll(func(f *flag.Flag) {
		if strings.HasPrefix(f.Usage, "internal:") || f.Name == "spec" {
			return
		}
		s.Fields = append(s.Fields, Field{Name: f.Name, Kind: kindOf(f.Value), List: repeatable(f.Value),
			JSON: slices.Contains(o.JSON, f.Name), Choices: o.Choices[f.Name], Default: f.DefValue, Usage: f.Usage})
	})
	return s
}

func kindOf(v flag.Value) string {
	if b, ok := v.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return "bool"
	}
	if g, ok := v.(flag.Getter); ok {
		switch g.Get().(type) {
		case int:
			return "int"
		case time.Duration:
			return "duration"
		}
	}
	return "string"
}

func repeatable(v flag.Value) bool {
	r, ok := v.(interface{ IsRepeatable() bool })
	return ok && r.IsRepeatable()
}

// Lookup returns the field named name.
func (s Schema) Lookup(name string) (Field, bool) {
	i := slices.IndexFunc(s.Fields, func(f Field) bool { return f.Name == name })
	if i < 0 {
		return Field{}, false
	}
	return s.Fields[i], true
}

// Names lists the field names in schema order.
func (s Schema) Names() []string {
	out := make([]string, len(s.Fields))
	for i, f := range s.Fields {
		out[i] = f.Name
	}
	return out
}

var current func() Schema

// Register installs ask's schema builder; ask calls it from init, so every binary that links ask has it.
func Register(build func() Schema) { current = sync.OnceValue(build) }

// Current is the registered schema; ok is false where ask is not linked (hooks unit tests).
func Current() (Schema, bool) {
	if current == nil {
		return Schema{}, false
	}
	return current(), true
}

// StubSchema swaps the registered schema for s and returns the restore func (tests only).
func StubSchema(s Schema) func() {
	prev := current
	current = func() Schema { return s }
	return func() { current = prev }
}

// Argv turns one spec object into ask argv (keys in schema order) and its prompt; every problem names the key.
func (s Schema) Argv(raw []byte) ([]string, string, error) {
	var doc map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil {
		return nil, "", fmt.Errorf("spec: not a JSON object: %w", err)
	}
	if doc == nil {
		return nil, "", errors.New("spec: not a JSON object")
	}
	if err := dec.Decode(&json.RawMessage{}); err != io.EOF {
		return nil, "", errors.New("spec: trailing data after the JSON object")
	}
	prompt := ""
	if v, ok := doc[PromptKey]; ok && json.Unmarshal(v, &prompt) != nil {
		return nil, "", fmt.Errorf("spec %s: want a string", PromptKey)
	}
	if err := s.CheckKeys(doc, PromptKey); err != nil {
		return nil, "", err
	}
	var argv []string
	for _, f := range s.Fields {
		if v, ok := doc[f.Name]; ok {
			w, err := f.words(v)
			if err != nil {
				return nil, "", err
			}
			argv = append(argv, w...)
		}
	}
	return argv, prompt, nil
}

// CheckKeys refuses the first key (sorted) neither in s nor in extra, naming the nearest valid key.
func (s Schema) CheckKeys(doc map[string]json.RawMessage, extra ...string) error {
	valid := append(s.Names(), extra...)
	for _, k := range slices.Sorted(maps.Keys(doc)) {
		if slices.Contains(valid, k) {
			continue
		}
		if n, ok := textutil.Nearest(k, valid); ok {
			return fmt.Errorf("spec key %q unknown: did you mean %q?", k, n)
		}
		return fmt.Errorf("spec key %q unknown (keys: %s)", k, strings.Join(valid, ", "))
	}
	return nil
}

func (f Field) words(v json.RawMessage) ([]string, error) {
	switch {
	case f.List:
		return f.listWords(v)
	case f.Kind == "bool":
		return f.boolWords(v)
	case f.Kind == "int":
		return f.intWords(v)
	case f.JSON && len(v) > 0 && v[0] == '{':
		var buf bytes.Buffer
		if err := json.Compact(&buf, v); err != nil {
			return nil, f.want("a JSON object")
		}
		return []string{"--" + f.Name, buf.String()}, nil
	}
	var str string
	if json.Unmarshal(v, &str) != nil {
		return nil, f.want("a string")
	}
	if err := f.CheckChoice(str); err != nil {
		return nil, err
	}
	return []string{"--" + f.Name, str}, nil
}

func (f Field) want(what string) error { return fmt.Errorf("spec %s: want %s", f.Name, what) }

func (f Field) listWords(v json.RawMessage) ([]string, error) {
	var xs []string
	if json.Unmarshal(v, &xs) != nil {
		return nil, f.want("an array of strings")
	}
	var out []string
	for _, x := range xs {
		out = append(out, "--"+f.Name, x)
	}
	return out, nil
}

func (f Field) boolWords(v json.RawMessage) ([]string, error) {
	var b bool
	if json.Unmarshal(v, &b) != nil {
		return nil, f.want("true or false")
	}
	return []string{"--" + f.Name + "=" + strconv.FormatBool(b)}, nil
}

func (f Field) intWords(v json.RawMessage) ([]string, error) {
	var n json.Number
	if json.Unmarshal(v, &n) != nil {
		return nil, f.want("an integer")
	}
	if _, err := n.Int64(); err != nil {
		return nil, f.want("an integer")
	}
	return []string{"--" + f.Name, n.String()}, nil
}

// CheckChoice refuses a value outside f.Choices, naming the nearest one.
func (f Field) CheckChoice(v string) error {
	if len(f.Choices) == 0 || slices.Contains(f.Choices, v) {
		return nil
	}
	if n, ok := textutil.Nearest(v, f.Choices); ok {
		return fmt.Errorf("--%s %q unknown: did you mean %q? (%s)", f.Name, v, n, strings.Join(f.Choices, "|"))
	}
	return fmt.Errorf("--%s %q unknown (%s)", f.Name, v, strings.Join(f.Choices, "|"))
}
