// Package settings seeds a missing settings.json and resets it on the owner's request; nothing else writes it.
package settings

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

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
	hooks, err := SettingsJSON()
	if err != nil {
		return nil, err
	}
	o.set("hooks", hooks)
	return o.marshal()
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
