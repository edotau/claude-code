package assets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Synthesis is what a model distils from source material before a skill is written; Go renders the skill from it.
type Synthesis struct {
	Principles       []string `json:"principles"`
	MentalModel      string   `json:"mental_model"`
	Heuristics       []string `json:"heuristics,omitempty"`
	Gotchas          []Gotcha `json:"gotchas"`
	ContextDependent []string `json:"context_dependent,omitempty"`
}

// Gotcha is one thing the skill's reader must not do, and why.
type Gotcha struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

// ParseSynthesis decodes and validates a synthesis record; unknown fields are an error so a typo is not dropped.
func ParseSynthesis(b []byte) (Synthesis, error) {
	var s Synthesis
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Synthesis{}, fmt.Errorf("synthesis: %w", err)
	}
	return s, s.Validate()
}

// Validate checks counts and that every item is one non-empty line.
func (s Synthesis) Validate() error {
	if n := len(s.Principles); n < 3 || n > 7 {
		return fmt.Errorf("synthesis: principles has %d items, want 3-7", n)
	}
	if len(s.Gotchas) == 0 {
		return errors.New("synthesis: gotchas needs at least one {what, why}")
	}
	lines := map[string][]string{"principles": s.Principles, "mental_model": {s.MentalModel},
		"heuristics": s.Heuristics, "context_dependent": s.ContextDependent}
	for _, g := range s.Gotchas {
		lines["gotchas"] = append(lines["gotchas"], g.What, g.Why)
	}
	for field, items := range lines {
		for _, v := range items {
			if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\r\n") {
				return fmt.Errorf("synthesis: %s needs non-empty single-line items", field)
			}
		}
	}
	return nil
}

// SynthesisExample is a filled-in record showing every field, for a model to copy and fill.
func SynthesisExample() string {
	ex := Synthesis{
		Principles:       []string{"<principle 1>", "<principle 2>", "<principle 3>"},
		MentalModel:      "<one line: how to think about the problem>",
		Heuristics:       []string{"<rule of thumb>"},
		Gotchas:          []Gotcha{{What: "<what not to do>", Why: "<why it breaks or misleads>"}},
		ContextDependent: []string{"<depends on X: do Y>"},
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep the <placeholder> brackets readable
	enc.SetIndent("", "  ")
	_ = enc.Encode(ex) // a fixed literal of strings cannot fail to encode
	return b.String()
}

// gotchaLines renders gotchas as "<what> — <why>" list items, the form the shape's Gotchas rule checks.
func gotchaLines(gs []Gotcha) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = strings.TrimRight(g.What, ".") + " — " + g.Why
	}
	return out
}
