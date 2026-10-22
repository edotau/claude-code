package settings

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// schemaProps loads the saved schemastore copy's top-level "properties" map once per test run.
func schemaProps(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/claude-code-settings.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Properties
}

// schemaEntry is the subset of a JSON-Schema property node this test cares about: type and enum, whether
// declared directly or inside an "anyOf" branch (theme uses anyOf for its custom: pattern branch).
type schemaEntry struct {
	Type  string        `json:"type"`
	Enum  []string      `json:"enum"`
	AnyOf []schemaEntry `json:"anyOf"`
}

func (e schemaEntry) kindAndEnum() (string, []string) {
	if e.Type != "" || len(e.Enum) > 0 {
		return e.Type, e.Enum
	}
	for _, sub := range e.AnyOf {
		if sub.Type != "" && len(sub.Enum) > 0 {
			return sub.Type, sub.Enum
		}
	}
	return e.Type, e.Enum
}

// TestCatalogSchemaEntriesMatch: every Catalog entry sourced from the schema must agree with the saved
// schema copy on JSON type and, where the schema declares one, the enum of allowed values.
func TestCatalogSchemaEntriesMatch(t *testing.T) {
	props := schemaProps(t)
	for _, f := range Catalog {
		if f.Source != "schema" {
			continue
		}
		raw, ok := props[f.Key]
		if !ok {
			t.Errorf("catalog entry %q claims Source=schema but the saved schema copy has no such property", f.Key)
			continue
		}
		var entry schemaEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("%s: decode schema property: %v", f.Key, err)
		}
		typ, enum := entry.kindAndEnum()
		checkKindMatchesSchemaType(t, f, typ)
		checkEnumMatchesCatalogValues(t, f, enum)
	}
}

// checkKindMatchesSchemaType asserts the catalog Kind agrees with the schema's declared JSON type.
func checkKindMatchesSchemaType(t *testing.T, f Field, typ string) {
	t.Helper()
	switch f.Kind {
	case KindBool:
		if typ != "boolean" {
			t.Errorf("%s: catalog Kind=Bool, schema type=%q", f.Key, typ)
		}
	case KindInt:
		if typ != "integer" && typ != "number" {
			t.Errorf("%s: catalog Kind=Int, schema type=%q", f.Key, typ)
		}
	case KindString, KindEnum:
		if typ != "string" {
			t.Errorf("%s: catalog Kind=String/Enum, schema type=%q", f.Key, typ)
		}
	case KindObject:
		if typ != "object" {
			t.Errorf("%s: catalog Kind=Object, schema type=%q", f.Key, typ)
		}
	}
}

// checkEnumMatchesCatalogValues asserts that, when the schema declares an enum, the catalog is KindEnum
// and every schema-allowed value is present in catalog.Values.
func checkEnumMatchesCatalogValues(t *testing.T, f Field, enum []string) {
	t.Helper()
	if len(enum) == 0 {
		return
	}
	if f.Kind != KindEnum {
		t.Errorf("%s: schema declares an enum but catalog Kind=%v, not KindEnum", f.Key, f.Kind)
	}
	if len(f.Values) == 0 {
		t.Errorf("%s: schema declares enum %v but catalog.Values is empty", f.Key, enum)
	}
	for _, v := range enum {
		if !slices.Contains(f.Values, v) {
			t.Errorf("%s: schema enum value %q is missing from catalog.Values %v", f.Key, v, f.Values)
		}
	}
}

// TestCatalogDocsAndTemplateEntriesAbsentFromSchema: entries sourced from docs or the template itself must
// NOT appear in the saved schema copy — the day schemastore catches up and adds one, this test starts
// failing and flags that the entry's Source should move to "schema".
func TestCatalogDocsAndTemplateEntriesAbsentFromSchema(t *testing.T) {
	props := schemaProps(t)
	for _, f := range Catalog {
		if f.Source != "docs" && f.Source != "template" {
			continue
		}
		if _, ok := props[f.Key]; ok {
			t.Errorf("catalog entry %q is marked Source=%q but the schema copy now defines it — move it to Source=\"schema\"", f.Key, f.Source)
		}
	}
}

// notEditable lists template keys deliberately excluded from Catalog: $schema is metadata, model has its
// own dedicated UI (plan "Out of scope").
var notEditable = map[string]bool{"$schema": true, "model": true}

// isScalar reports whether a raw JSON value is a plain scalar (bool/number/string), not an object or array —
// Catalog only covers scalar template keys; env/permissions/statusLine/enabledPlugins/extraKnownMarketplaces
// are structured and out of scope for this page.
func isScalar(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch v.(type) {
	case bool, float64, string:
		return true
	default:
		return false
	}
}

// TestEveryScalarTemplateKeyIsCataloged: every scalar key the rendered template owns is either in Catalog
// or on the short not-editable list — nothing should silently fall through both.
func TestEveryScalarTemplateKeyIsCataloged(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rendered, &obj); err != nil {
		t.Fatal(err)
	}
	cataloged := map[string]bool{}
	for _, f := range Catalog {
		cataloged[f.Key] = true
	}
	for k, raw := range obj {
		if k == "hooks" {
			continue // hookspec-contributed, not a template key
		}
		if !isScalar(raw) {
			continue
		}
		if notEditable[k] {
			continue
		}
		if !cataloged[k] {
			t.Errorf("scalar template key %q is neither in Catalog nor on the not-editable list", k)
		}
	}
}

// TestCatalogSectionsMatchSections: a Catalog entry's Section must name the Sections() entry that actually
// owns its key, so the configure page groups rows under the right header. Section=="" is the overlay-only
// escape hatch (a key the template never renders, so no Sections() entry can own it) — restricted to
// KindObject so a template key can't silently lose its section this way.
func TestCatalogSectionsMatchSections(t *testing.T) {
	sections, err := Sections()
	if err != nil {
		t.Fatal(err)
	}
	owner := map[string]string{}
	for _, s := range sections {
		for _, k := range s.Keys {
			owner[k] = s.Name
		}
	}
	for _, f := range Catalog {
		if f.Section == "" {
			if f.Kind != KindObject {
				t.Errorf("catalog entry %q: Section=\"\" but Kind=%v, want KindObject (overlay-only)", f.Key, f.Kind)
			}
			if _, owned := owner[f.Key]; owned {
				t.Errorf("catalog entry %q: Section=\"\" but Sections() still owns it as %q", f.Key, owner[f.Key])
			}
			continue
		}
		want, ok := owner[f.Key]
		if !ok {
			t.Errorf("catalog entry %q: no Sections() entry owns this key", f.Key)
			continue
		}
		if f.Section != want {
			t.Errorf("catalog entry %q: Section=%q, but Sections() assigns it to %q", f.Key, f.Section, want)
		}
	}
}

// TestLookupFindsCataloged: Lookup must resolve every key actually present in Catalog, and report absent
// for one that cannot be.
func TestLookupFindsCataloged(t *testing.T) {
	if len(Catalog) == 0 {
		t.Fatal("Catalog is empty")
	}
	want := Catalog[0]
	got, ok := Lookup(want.Key)
	if !ok || got.Key != want.Key {
		t.Errorf("Lookup(%q) = %+v, %v; want the catalog entry", want.Key, got, ok)
	}
	if _, ok := Lookup("this-key-does-not-exist"); ok {
		t.Error("Lookup(unknown key) = true, want false")
	}
}

// TestParseRejectsCleanupPeriodDaysZero: the schema's minimum is 1; "0" must be rejected, not merely
// discouraged — a 0-day retention would behave as "delete everything immediately".
func TestParseRejectsCleanupPeriodDaysZero(t *testing.T) {
	f, ok := Lookup("cleanupPeriodDays")
	if !ok {
		t.Fatal("cleanupPeriodDays is not in Catalog")
	}
	if _, err := f.Parse("0"); err == nil {
		t.Error("Parse(\"0\") on cleanupPeriodDays = nil error, want rejection (minimum is 1)")
	}
	if _, err := f.Parse("30"); err != nil {
		t.Errorf("Parse(\"30\") on cleanupPeriodDays = %v, want no error", err)
	}
}

// TestValidateRejectsNull: a JSON null must never satisfy any Kind's Validate — json.Unmarshal(null, ptr)
// silently succeeds as a no-op, so without an explicit null guard every Kind would accept it unchallenged.
func TestValidateRejectsNull(t *testing.T) {
	for _, key := range []string{"alwaysThinkingEnabled", "effortLevel", "outputStyle", "cleanupPeriodDays"} {
		f, ok := Lookup(key)
		if !ok {
			t.Fatalf("%s is not in Catalog", key)
		}
		if err := f.Validate(json.RawMessage("null")); err == nil {
			t.Errorf("%s (Kind=%v): Validate(null) = nil error, want rejection", key, f.Kind)
		}
	}
}

// TestValidateAutoModeAcceptsObject: autoMode (KindObject) accepts any JSON object, rejects non-objects.
func TestValidateAutoModeAcceptsObject(t *testing.T) {
	f, ok := Lookup("autoMode")
	if !ok {
		t.Fatal("autoMode is not in Catalog")
	}
	if err := f.Validate(json.RawMessage(`{"soft_deny":["$defaults"]}`)); err != nil {
		t.Errorf("Validate(object) = %v, want no error", err)
	}
	for _, bad := range []string{`[]`, `"x"`, `null`} {
		if err := f.Validate(json.RawMessage(bad)); err == nil {
			t.Errorf("Validate(%s) = nil error, want rejection", bad)
		}
	}
}

// TestParseRejectsAutoMode: autoMode has no text-entry form; Parse always errors, directing to vault edit.
func TestParseRejectsAutoMode(t *testing.T) {
	f, ok := Lookup("autoMode")
	if !ok {
		t.Fatal("autoMode is not in Catalog")
	}
	if _, err := f.Parse(`{"soft_deny":["$defaults"]}`); err == nil {
		t.Error("Parse on autoMode = nil error, want rejection (no text-entry form)")
	}
}

// TestParseRejectsEffortLevelMax: "max" and "ultracode" are session-only per the schema description and are
// not accepted in the settings file — only low/medium/high/xhigh persist.
func TestParseRejectsEffortLevelMax(t *testing.T) {
	f, ok := Lookup("effortLevel")
	if !ok {
		t.Fatal("effortLevel is not in Catalog")
	}
	if _, err := f.Parse("max"); err == nil {
		t.Error("Parse(\"max\") on effortLevel = nil error, want rejection (session-only value)")
	}
	if _, err := f.Parse("high"); err != nil {
		t.Errorf("Parse(\"high\") on effortLevel = %v, want no error", err)
	}
}
