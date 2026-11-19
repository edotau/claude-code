package shell

import (
	"bytes"
	_ "embed"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
)

// guardsJSON is compiled in and read from nowhere else: no setting, variable or vault entry overrides it.
//
//go:embed guards.json
var guardsJSON []byte

// guardSection is one named list of words or one tool -> words map, with its one-line purpose.
type guardSection struct {
	Purpose string              `json:"purpose"`
	Words   []string            `json:"words"`
	Tools   map[string][]string `json:"tools"`
}

// guardExclusive are section pairs whose words must never overlap: a word in both is a contradiction.
var guardExclusive = [][2]string{
	{"rebase_resume_flags", "rebase_value_flags"},
	{"patch_write_tools", "formatter_write_flags"},
	{"removal_operand_commands", "removal_move_commands"},
	{"writer_script_flags", "writer_operand_commands"},
	{"writer_script_flags", "writer_destination_commands"},
	{"writer_operand_commands", "writer_destination_commands"},
	{"find_exec_flags", "find_write_flags"},
	{"data_commands", "runner_names"},
	{"inert_commands", "evaluators"},
	{"pipe_filters", "evaluators"},
}

// parsedGuards is guards.json decoded and validated once per process; a malformed file panics at first use.
var parsedGuards = sync.OnceValue(func() map[string]guardSection {
	secs, err := decodeGuards(guardsJSON)
	if err != nil {
		panic("shell: guards.json: " + err.Error())
	}
	return secs
})

func decodeGuards(raw []byte) (map[string]guardSection, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw), jsontext.AllowDuplicateNames(true))
	secs, err := decodeSections(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.ReadToken(); err != io.EOF {
		return nil, errors.New("trailing data after the top-level object")
	}
	for _, name := range slices.Sorted(maps.Keys(secs)) {
		if err := secs[name].validate(); err != nil {
			return nil, fmt.Errorf("section %q: %w", name, err)
		}
	}
	for _, pair := range guardExclusive {
		a, b := secs[pair[0]], secs[pair[1]]
		inB := b.names()
		for w := range a.names() {
			if _, both := inB[w]; both {
				return nil, fmt.Errorf("%q is in %s and %s, which are exclusive", w, pair[0], pair[1])
			}
		}
	}
	return secs, nil
}

func expectKind(dec *jsontext.Decoder, want jsontext.Kind) (jsontext.Token, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return tok, err
	}
	if tok.Kind() != want {
		return tok, fmt.Errorf("want %q, got %q", want, tok.Kind())
	}
	return tok, nil
}

// objectKeys calls each with every key of the object at the decoder; each must consume that key's value.
func objectKeys(dec *jsontext.Decoder, each func(key string) error) error {
	if _, err := expectKind(dec, '{'); err != nil {
		return err
	}
	for dec.PeekKind() != '}' {
		key, err := expectKind(dec, '"')
		if err != nil {
			return err
		}
		if err := each(key.String()); err != nil {
			return err
		}
	}
	_, err := expectKind(dec, '}')
	return err
}

func decodeSections(dec *jsontext.Decoder) (map[string]guardSection, error) {
	secs := map[string]guardSection{}
	err := objectKeys(dec, func(name string) error {
		sec, err := decodeSection(dec)
		secs[name] = sec
		return err
	})
	return secs, err
}

func decodeSection(dec *jsontext.Decoder) (guardSection, error) {
	var s guardSection
	err := objectKeys(dec, func(key string) (err error) {
		switch key {
		case "purpose":
			var tok jsontext.Token
			if tok, err = expectKind(dec, '"'); err == nil {
				s.Purpose = tok.String()
			}
		case "words":
			s.Words, err = decodeWords(dec)
		case "tools":
			s.Tools = map[string][]string{}
			err = objectKeys(dec, func(tool string) (err error) {
				s.Tools[tool], err = decodeWords(dec)
				return err
			})
		default:
			err = fmt.Errorf("json: unknown field %q", key)
		}
		return err
	})
	return s, err
}

func decodeWords(dec *jsontext.Decoder) ([]string, error) {
	if _, err := expectKind(dec, '['); err != nil {
		return nil, err
	}
	words := []string{}
	for dec.PeekKind() != ']' {
		tok, err := expectKind(dec, '"')
		if err != nil {
			return nil, err
		}
		words = append(words, tok.String())
	}
	_, err := expectKind(dec, ']')
	return words, err
}

func (s guardSection) validate() error {
	if s.Purpose == "" {
		return errors.New("empty purpose")
	}
	if (len(s.Words) == 0) == (len(s.Tools) == 0) {
		return errors.New("needs exactly one non-empty words list or tools map")
	}
	if err := checkWords(s.Words); len(s.Words) > 0 && err != nil {
		return err
	}
	for tool, words := range s.Tools {
		if len(words) == 0 {
			return fmt.Errorf("tool %q has no words", tool)
		}
		if err := checkWords(words); err != nil {
			return fmt.Errorf("tool %q: %w", tool, err)
		}
	}
	return nil
}

func checkWords(words []string) error {
	seen := map[string]bool{}
	for _, w := range words {
		if w == "" {
			return errors.New("empty word")
		}
		if seen[w] {
			return fmt.Errorf("duplicate word %q", w)
		}
		seen[w] = true
	}
	return nil
}

// names are the section's words, or its tool names when it maps tools.
func (s guardSection) names() map[string]struct{} {
	out := map[string]struct{}{}
	for _, w := range s.Words {
		out[w] = struct{}{}
	}
	for t := range s.Tools {
		out[t] = struct{}{}
	}
	return out
}

func guardSectionNamed(name string) guardSection {
	s, ok := parsedGuards()[name]
	if !ok {
		panic("shell: guards.json has no section " + name)
	}
	return s
}

// GuardWords is a copy of the section's words, in file order.
func GuardWords(name string) []string { return slices.Clone(guardSectionNamed(name).Words) }

// GuardSet is the section's words as a set.
func GuardSet(name string) map[string]bool {
	words := guardSectionNamed(name).Words
	out := make(map[string]bool, len(words))
	for _, w := range words {
		out[w] = true
	}
	return out
}

// GuardTools is a copy of the section's tool -> words map.
func GuardTools(name string) map[string][]string {
	tools := guardSectionNamed(name).Tools
	out := make(map[string][]string, len(tools))
	for t, words := range tools {
		out[t] = slices.Clone(words)
	}
	return out
}

// GuardToolSets is the section's tool -> words map with each list as a set.
func GuardToolSets(name string) map[string]map[string]bool {
	tools := guardSectionNamed(name).Tools
	out := make(map[string]map[string]bool, len(tools))
	for t, words := range tools {
		set := make(map[string]bool, len(words))
		for _, w := range words {
			set[w] = true
		}
		out[t] = set
	}
	return out
}
