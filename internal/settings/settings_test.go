package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
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
		`{"teammateDefaultModel": null}`:    "",
		`{"model": null}`:                   "model: want a value, got null",
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
	if testing.Short() {
		t.Skip("pre-ship tier: whole-repo AST walk")
	}
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
	want := []string{"internal/settings/settings.go:ResetSettings", "internal/settings/settings.go:SeedSettings"}
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

// TestWriteBackupNeverTruncatesExisting checks a second backup under the same name fails and keeps the first.
func TestWriteBackupNeverTruncatesExisting(t *testing.T) {
	name := filepath.Join(t.TempDir(), "settings.json.bak.x")
	if err := writeBackup(name, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeBackup(name, []byte("second"), 0o644); !os.IsExist(err) {
		t.Fatalf("second writeBackup err = %v, want exists", err)
	}
	if got, _ := os.ReadFile(name); string(got) != "first" {
		t.Errorf("backup = %q, want first", got)
	}
}

func TestLivePermissionsReadsListsAndRejectsBadFiles(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if _, err := LivePermissions(); err == nil {
		t.Error("missing file: want an error")
	}
	file := filepath.Join(cfg, "settings.json")
	wantDeny := map[string]int{ // -1 means an error is expected
		`{"permissions":{"deny":["a"],"ask":["b"],"allow":["c"]}}`: 1, `{"permissions":{"deny":[]}}`: 0, `{bad`: -1,
		`null`: -1, `{}`: -1, `{"permissions":null}`: -1, `{"permissions":{}}`: -1,
		`{"permissions":{"deny":null}}`: -1, `{"permissions":"x"}`: -1, `{"permissions":{"deny":"x"}}`: -1,
		`{"permissions":{"deny":[1]}}`: -1,
	}
	for body, want := range wantDeny {
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LivePermissions()
		if (want < 0) != (err != nil) || (err == nil && len(got.Deny) != want) {
			t.Errorf("%s: deny = %v, err = %v, want %d", body, got.Deny, err, want)
		}
	}
}

func TestLivePermissionsDecodesAllowAndAsk(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	file := filepath.Join(cfg, "settings.json")
	for _, tc := range []struct {
		body       string
		allow, ask []string
	}{
		{`{"permissions":{"deny":[],"allow":["a"],"ask":["b","c"]}}`, []string{"a"}, []string{"b", "c"}},
		{`{"permissions":{"deny":[]}}`, nil, nil},
	} {
		if err := os.WriteFile(file, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LivePermissions()
		want := PermissionLists{Allow: tc.allow, Ask: tc.ask, Deny: []string{}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v, err %v, want %+v", tc.body, got, err, want)
		}
	}
	if err := os.WriteFile(file, []byte(`{"permissions":{"allow":["a"],"ask":["b"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LivePermissions(); err == nil {
		t.Error("allow and ask without deny: want an error")
	}
}

func TestFormatOrdersAndKeepsHookOrder(t *testing.T) {
	in := `{"permissions":{"allow":["b","$defaults","A"]},"env":{"Z":"1","A":"2"},"$schema":"x",
"hooks":{"Stop":[{"hooks":[{"timeout":5,"command":"z & y","type":"command"}]},{"matcher":"b"}]}}`
	got, err := Format([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{"\"$defaults\",\n      \"A\",\n      \"b\"", "\"A\": \"2\",\n    \"Z\": \"1\"", `"command": "z & y"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Index(s, `"$schema"`) > strings.Index(s, `"permissions"`) || strings.Index(s, `"z & y"`) > strings.Index(s, `"matcher"`) {
		t.Errorf("order wrong:\n%s", s)
	}
	if p, err := Unformatted(got); err != nil || p != "" {
		t.Errorf("Format output not stable: %q %v", p, err)
	}
	if _, err := Format([]byte("{\n\"a\": }")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("want located error, got %v", err)
	}
}

func TestFormatPreservesRawValues(t *testing.T) {
	for _, tc := range []struct {
		name, input, path string
		want              []string
	}{
		{"numbers", `{"x":[9007199254740993,1e1000,1.00,1E+09,-0]}`, "(whitespace)",
			[]string{"9007199254740993", "1e1000", "1.00", "1E+09", "-0"}},
		{"mixed", `{"x":["b",null,"a"]}`, "(whitespace)", []string{"\"b\",\n    null,\n    \"a\""}},
		{"empty", `{"x":[{},[],null]}`, "(whitespace)", []string{"{}", "[]", "null"}},
		{"unicode", `{"x":["\u00e9 <>&"]}`, "(whitespace)", []string{`"\u00e9 <>&"`}},
		{"defaults", `{"permissions":{"allow":["!","$defaults"]}}`, "permissions.allow[0]",
			[]string{"\"$defaults\",\n      \"!\""}},
		{"refs", `{"hooks":{"Stop":[{"hooks":[{"timeout":5,"command":"x","type":"command"}]}]}}`,
			"hooks.Stop[0].hooks[0].timeout", []string{"\"type\": \"command\",\n            \"command\": \"x\",\n            \"timeout\": 5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Format([]byte(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(out), want) {
					t.Errorf("missing %q in %s", want, out)
				}
			}
			if path, err := Unformatted([]byte(tc.input)); err != nil || path != tc.path {
				t.Errorf("path = %q, err = %v; want %q", path, err, tc.path)
			}
			if path, err := Unformatted(out); err != nil || path != "" {
				t.Errorf("unstable output: %q, %v", path, err)
			}
		})
	}
}
