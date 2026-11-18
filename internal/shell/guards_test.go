package shell

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestGuardsFileIsValid(t *testing.T) {
	secs, err := decodeGuards(guardsJSON)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(guardsJSON))
	dec.Token()
	seen := map[string]bool{}
	for dec.More() {
		tok, _ := dec.Token()
		name := tok.(string)
		if seen[name] {
			t.Errorf("duplicate section %q", name)
		}
		seen[name] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != len(secs) {
		t.Errorf("%d keys decoded to %d sections", len(seen), len(secs))
	}
	for _, pair := range guardSubsets {
		for w := range secs[pair[0]].names() {
			if _, ok := secs[pair[1]].names()[w]; !ok {
				t.Errorf("%q is in %s but not in %s", w, pair[0], pair[1])
			}
		}
	}
}

// guardSubsets are section pairs whose first names (words or tools) must all be in the second.
var guardSubsets = [][2]string{
	{"relay_go_bool_flags", "relay_go_value_flags"},
	{"git_read_only_gated_verbs", "git_read_only_verbs"},
	{"git_shared_tree_banned_verbs", "git_alias_proof_verbs"},
}

func TestDecodeGuardsRejects(t *testing.T) {
	a, b := guardExclusive[0][0], guardExclusive[0][1]
	cases := map[string]string{
		"unknown field":   `{"x":{"purpose":"p","words":["a"],"extra":1}}`,
		"empty purpose":   `{"x":{"purpose":"","words":["a"]}}`,
		"empty list":      `{"x":{"purpose":"p","words":[]}}`,
		"both shapes":     `{"x":{"purpose":"p","words":["a"],"tools":{"t":["a"]}}}`,
		"duplicate word":  `{"x":{"purpose":"p","words":["a","b","a"]}}`,
		"empty word":      `{"x":{"purpose":"p","words":[""]}}`,
		"empty tool list": `{"x":{"purpose":"p","tools":{"t":[]}}}`,
		"duplicate tool":  `{"x":{"purpose":"p","tools":{"t":["a","a"]}}}`,
		"exclusive pair": fmt.Sprintf(`{%q:{"purpose":"p","words":["a","c"]},%q:{"purpose":"p","words":["b","a"]}}`,
			a, b),
	}
	for name, doc := range cases {
		if _, err := decodeGuards([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGuardAccessorsReturnCopies(t *testing.T) {
	words := GuardWords("rebase_resume_flags")
	words[0] = "changed"
	if GuardWords("rebase_resume_flags")[0] == "changed" {
		t.Error("GuardWords shares its slice")
	}
	tools := GuardTools("python_runner_options")
	tools["uv"][0] = "changed"
	if GuardTools("python_runner_options")["uv"][0] == "changed" {
		t.Error("GuardTools shares its slices")
	}
}

func TestGuardMissingSectionPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a read of a missing section did not panic")
		}
	}()
	GuardWords("no_such_section")
}

// Every section is read by some accessor call and every call names a section: no dead list, no typo.
func TestGuardSectionsMatchReaders(t *testing.T) {
	call := regexp.MustCompile(`\bGuard(?:Words|Set|Tools|ToolSets)\("([a-z0-9_]+)"\)`)
	read := map[string]bool{}
	err := filepath.WalkDir("..", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		src, err := os.ReadFile(p)
		for _, m := range call.FindAllSubmatch(src, -1) {
			read[string(m[1])] = true
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range parsedGuards() {
		if !read[name] {
			t.Errorf("section %q is read by no accessor call", name)
		}
	}
	for name := range read {
		if _, ok := parsedGuards()[name]; !ok {
			t.Errorf("accessor call names missing section %q", name)
		}
	}
}
