package shell

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
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
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var secs map[string]guardSection
	if err := dec.Decode(&secs); err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(secs)) {
		if err := secs[name].validate(); err != nil {
			return nil, fmt.Errorf("section %q: %w", name, err)
		}
	}
	for _, pair := range guardExclusive {
		a, b := secs[pair[0]], secs[pair[1]]
		for w := range a.names() {
			if _, both := b.names()[w]; both {
				return nil, fmt.Errorf("%q is in %s and %s, which are exclusive", w, pair[0], pair[1])
			}
		}
	}
	return secs, nil
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
