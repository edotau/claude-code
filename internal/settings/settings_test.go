package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRenderValidJSON(t *testing.T) {
	got, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(got) {
		t.Error("render is not valid JSON")
	}
}

func TestAdditionalPropertiesSchema(t *testing.T) {
	k, ok := schemaKey("enabledPlugins")
	if !ok {
		t.Skip("no enabledPlugins in schema")
	}
	if err := k.Validate(json.RawMessage(`{"plugin@marketplace":7}`)); err == nil {
		t.Error("non-boolean plugin value accepted")
	}
	if err := k.Validate(json.RawMessage(`{"plugin@marketplace":true}`)); err != nil {
		t.Errorf("valid value rejected: %v", err)
	}
}

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

// TestSchemaKeyValidateRejects: an unknown key is absent and a wrong type or out-of-enum value fails.
func TestSchemaKeyValidateRejects(t *testing.T) {
	if _, ok := schemaKey("noSuchSetting"); ok {
		t.Error("schemaKey(noSuchSetting) found a key")
	}
	cases := map[string]string{"tui": `"sideways"`, "cleanupPeriodDays": `"30"`, "prefersReducedMotion": `1`, "permissions": `[]`}
	for name, raw := range cases {
		k, ok := schemaKey(name)
		if !ok {
			t.Fatalf("%s missing from schema", name)
		}
		if k.Validate(json.RawMessage(raw)) == nil {
			t.Errorf("%s accepted %s", name, raw)
		}
	}
	if k, _ := schemaKey("theme"); k.Validate(json.RawMessage(`"custom:mine"`)) != nil {
		t.Error("theme rejected custom:mine")
	}
	if k, _ := schemaKey("CLAUDE_CODE_SCROLL_SPEED"); !k.Env {
		t.Error("CLAUDE_CODE_SCROLL_SPEED not an env key")
	}
}

func TestKeyValidateRefusesUncompiledPattern(t *testing.T) {
	k := Key{Name: "x", Kind: kindString, patternErr: errors.New("bad")}
	if err := k.Validate(json.RawMessage(`"a"`)); err == nil {
		t.Error("a key whose pattern failed to compile validated a value")
	}
}

func TestSchemaNumbersBoundsAndUnions(t *testing.T) {
	ok := func(key, val string, want bool) {
		t.Helper()
		k, found := schemaKey(key)
		if !found {
			t.Fatalf("no schema key %s", key)
		}
		if err := k.Validate(json.RawMessage(val)); (err == nil) != want {
			t.Errorf("%s=%s: err = %v, want accepted=%v", key, val, err, want)
		}
	}
	ok("feedbackSurveyRate", "0.5", true)
	ok("feedbackSurveyRate", "1.5", false)     // maximum
	ok("skillListingMaxDescChars", "0", false) // exclusiveMinimum
	ok("skillListingMaxDescChars", "5", true)
	ok("skillListingBudgetFraction", "0", false)
	ok("skillListingBudgetFraction", "2", false)
	ok("strictPluginOnlyCustomization", "true", true) // union: boolean branch
	ok("strictPluginOnlyCustomization", `["skills"]`, true)
	ok("strictPluginOnlyCustomization", `"x"`, false)
	ok("forceLoginOrgUUID", `["a","b"]`, true) // oneOf: array branch
	ok("forceLoginOrgUUID", "3", false)
	ok("forceLoginOrgUUID", `[3]`, false) // array items
	ok("strictPluginOnlyCustomization", `["bogus"]`, false)
	ok("strictPluginOnlyCustomization", `["hooks"]`, true)
	ok("env", `{"MAX_THINKING_TOKENS":5}`, false) // nested property
	ok("theme", `"custom:mine"`, true)
	ok("theme", `"custom:"`, false)

}

func TestObjectConstraintsEnforced(t *testing.T) {
	k, _ := schemaKey("subagentStatusLine")
	for val, want := range map[string]bool{
		`{"type":"command","command":"c"}`:           true,
		`{"command":"c"}`:                            false, // required
		`{"type":"other","command":"c"}`:             false, // const
		`{"type":"command","command":"c","extra":1}`: false, // additionalProperties
	} {
		if err := k.Validate(json.RawMessage(val)); (err == nil) != want {
			t.Errorf("subagentStatusLine=%s: err = %v, want accepted=%v", val, err, want)
		}
	}
}

// TestCheckReportsSyntaxLocationAndSchemaErrors: valid passes, unknown keys pass, a bad value or bad JSON is named.
func TestCheckReportsSyntaxLocationAndSchemaErrors(t *testing.T) {
	for in, want := range map[string]string{
		`{"model": "opus", "zzMine": [1]}`:  "",
		`{"cleanupPeriodDays": "7"}`:        "cleanupPeriodDays: want a number",
		"{\n  \"model\": \"x\",\n  oops\n}": "line 3 col 4",
		`[]`:                                "cannot unmarshal array",
		`null`:                              "not a JSON object",
	} {
		err := Check([]byte(in))
		if (want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("Check(%q) = %v, want %q", in, err, want)
		}
	}
}

// TestRenderHasOnlySeedAndResetCallers: any production Render reference beyond the seed and the reset is a new writer.
func TestRenderHasOnlySeedAndResetCallers(t *testing.T) {
	root := filepath.Join("..", "..")
	var got []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return skipDir(err, d, path != root)
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		got = append(got, renderRefs(t, path, filepath.ToSlash(rel))...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{"internal/settings/apply.go:ResetSettings", "internal/settings/apply.go:SeedSettings"}
	if !slices.Equal(got, want) {
		t.Errorf("Render referenced from %v, want only %v", got, want)
	}
}

// skipDir passes a walk error through and skips testdata and dot directories below the root.
func skipDir(err error, d fs.DirEntry, below bool) error {
	if err == nil && below && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
		return filepath.SkipDir
	}
	return err
}

// renderRefs lists file:func for every reference to settings.Render (or bare Render inside package settings).
func renderRefs(t *testing.T, path, rel string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	inPkg := f.Name.Name == "settings" && strings.HasPrefix(rel, "internal/settings/")
	var refs []string
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil && refersToRender(fn.Body, inPkg) {
			refs = append(refs, rel+":"+fn.Name.Name)
		}
	}
	return refs
}

// refersToRender reports a settings.Render selector, or a bare Render ident when inPkg, anywhere under body.
func refersToRender(body ast.Node, inPkg bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			found = found || ok && id.Name == "settings" && x.Sel.Name == "Render"
			return false
		case *ast.Ident:
			found = found || inPkg && x.Name == "Render"
		}
		return !found
	})
	return found
}
