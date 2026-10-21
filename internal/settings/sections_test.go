package settings

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// TestRenderSchemaFirst: $schema must stay the first key emitted — install/upgrade tooling and humans
// reading settings.json expect it leading the file.
func TestRenderSchemaFirst(t *testing.T) {
	got, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(got))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("render did not start with an object: %v %v", tok, err)
	}
	tok, err := dec.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "$schema" {
		t.Errorf("first key = %q, want $schema", tok)
	}
}

// TestSectionsCoverRenderedKeys: every top-level key Render emits from the section files (all but
// "hooks", which hookspec contributes) must come from exactly one Section — never zero, never two.
func TestSectionsCoverRenderedKeys(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rendered, &obj); err != nil {
		t.Fatal(err)
	}
	sections, err := Sections()
	if err != nil {
		t.Fatal(err)
	}
	owner := map[string][]string{}
	for _, s := range sections {
		for _, k := range s.Keys {
			owner[k] = append(owner[k], s.Name)
		}
	}
	for k := range obj {
		switch k {
		case "hooks": // hookspec.SettingsJSON contributes this key, not a section file
			continue
		case "$schema": // Sections() deliberately excludes it (schema metadata, not a setting)
			continue
		}
		switch owners := owner[k]; len(owners) {
		case 0:
			t.Errorf("rendered key %q is not owned by any Section", k)
		case 1:
			// exactly one owner, as required
		default:
			t.Errorf("rendered key %q owned by %d sections: %v", k, len(owners), owners)
		}
	}
	for k, owners := range owner {
		if len(owners) > 1 {
			t.Errorf("section key %q duplicated across sections: %v", k, owners)
		}
		if _, ok := obj[k]; !ok {
			t.Errorf("section %v declares key %q that Render never emits", owners, k)
		}
	}
}

// TestMergeSectionsRejectsDuplicateKey: a top-level key defined in two section files is an error
// naming both files, not a silent overwrite.
func TestMergeSectionsRejectsDuplicateKey(t *testing.T) {
	fsys := fstest.MapFS{
		"template/00-a.json": {Data: []byte(`{"theme":"dark"}`)},
		"template/10-b.json": {Data: []byte(`{"theme":"light"}`)},
	}
	_, err := mergeSections(fsys)
	if err == nil {
		t.Fatal("mergeSections returned no error for a key defined in two files")
	}
	if !strings.Contains(err.Error(), "00-a.json") || !strings.Contains(err.Error(), "10-b.json") {
		t.Errorf("error %q does not name both files", err)
	}
}

// TestMergeSectionsMergesDisjointKeysInNameOrder: section files with no overlapping keys merge into
// one object, keys ordered by file name.
func TestMergeSectionsMergesDisjointKeysInNameOrder(t *testing.T) {
	fsys := fstest.MapFS{
		"template/10-b.json": {Data: []byte(`{"beta":"two"}`)},
		"template/00-a.json": {Data: []byte(`{"alpha":"one"}`)},
	}
	o, err := mergeSections(fsys)
	if err != nil {
		t.Fatalf("mergeSections: %v", err)
	}
	if want := []string{"alpha", "beta"}; !slices.Equal(o.keys, want) {
		t.Errorf("keys = %v, want %v (file-name order)", o.keys, want)
	}
	if string(o.vals["alpha"]) != `"one"` || string(o.vals["beta"]) != `"two"` {
		t.Errorf("vals = %v", o.vals)
	}
}
