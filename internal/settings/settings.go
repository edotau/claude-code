// Package settings seeds a missing settings.json and resets it on the owner's request; nothing else writes it.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/defaults"
	"github.com/edotau/claude-code/internal/textutil"
)

// Section groups the default keys shown under one configure-page header.
type Section struct {
	Name string
	Keys []string
}

// sectionOrder is the header order; structuralSection homes the schema keys the catalog does not section.
var (
	sectionOrder      = []string{"model", "ui", "notifications", "session", "env", "more"}
	structuralSection = map[string]string{"model": "model"}
)

// Sections groups the editable keys by section: the defaults file's scalar keys and env variables first,
// then every sectioned catalog key the file lacks, so a key removed from the file stays on the page.
func Sections() ([]Section, error) {
	o, err := parseObject(defaults.Claude)
	if err != nil {
		return nil, fmt.Errorf("defaults/claude/settings.json: %w", err)
	}
	state := sectionState{by: map[string][]string{}, seen: map[string]bool{}}
	for _, k := range o.keys {
		if err := state.addDefault(k, o.vals[k]); err != nil {
			return nil, err
		}
	}
	for _, f := range Catalog() {
		if f.Section != "" {
			state.add(f.Section, f.Key)
		}
	}
	sections := make([]Section, 0, len(sectionOrder))
	for _, name := range sectionOrder {
		if keys := state.by[name]; len(keys) > 0 {
			sections = append(sections, Section{Name: name, Keys: keys})
		}
	}
	return sections, nil
}

type sectionState struct {
	by   map[string][]string
	seen map[string]bool
}

func (s *sectionState) add(name, key string) {
	if !s.seen[key] {
		s.seen[key] = true
		s.by[name] = append(s.by[name], key)
	}
}

func (s *sectionState) addDefault(k string, raw json.RawMessage) error {
	if k == "env" {
		return s.addEnv(raw)
	}
	f, ok := FieldFor(k)
	if !ok || f.Kind == KindObject || f.Kind == KindArray || CredentialKey(k) {
		return nil
	}
	name := f.Section
	if name == "" {
		name = structuralSection[k]
	}
	if name == "" {
		name = "more"
	}
	s.add(name, k)
	return nil
}

func (s *sectionState) addEnv(raw json.RawMessage) error {
	env, err := parseObject(raw)
	if err != nil {
		return fmt.Errorf("defaults/claude/settings.json env: %w", err)
	}
	for _, e := range env.keys {
		if _, ok := FieldFor(e); ok && !CredentialKey(e) {
			s.add("env", e)
		}
	}
	return nil
}

// Template is the embedded settings template, read-only: the harness keys a fresh settings.json starts from.
func Template() []byte { return defaults.Claude }

// loadDefaults parses the embedded defaults file, expanding ${HOOK_BIN}.
func loadDefaults() (*object, error) {
	expanded := strings.ReplaceAll(string(Template()), "${HOOK_BIN}", HookBin)
	o, err := parseObject([]byte(expanded))
	if err != nil {
		return nil, fmt.Errorf("defaults/claude/settings.json: %w", err)
	}
	return o, nil
}

// Render is the template plus the hook registry block; only SeedSettings and ResetSettings may call it.
func Render() ([]byte, error) {
	o, err := loadDefaults()
	if err != nil {
		return nil, err
	}
	hooks, err := SettingsJSON()
	if err != nil {
		return nil, err
	}
	o.set("hooks", hooks)
	return o.marshal()
}

// object is a JSON object that remembers key order, so a merge leaves the user's layout intact.
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

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}
