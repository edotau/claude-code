package settings

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Kind is the shape a Field's value takes in settings.json.
type Kind int

const (
	KindBool Kind = iota
	KindEnum
	KindInt
	KindString
	KindObject
	KindNumber
	KindArray
)

// Field describes one editable default key: its section, its value shape, and where its
// allowed values came from. Kind, Values and Min come from SchemaKey unless Source is "docs".
type Field struct {
	Key     string
	Section string
	Kind    Kind
	Values  []string // KindEnum only
	Min     int      // KindInt only
	Source  string   // "schema" | "docs" | "template"
}

// Catalog lists every plain-value default key except "model" and "$schema". Source "schema"
// means the schemastore copy in claude-code-settings.schema.json documents it; "docs"
// means code.claude.com documents it but schemastore is behind; "template" means neither does.
var Catalog = resolveSchema(catalogSpec)

// catalogSpec carries Key, Section and Source; docs-only entries also carry their hand-written Kind and Values.
var catalogSpec = []Field{
	{Key: "outputStyle", Section: "model", Source: "schema"},
	{Key: "switchModelsOnFlag", Section: "model", Kind: KindBool, Source: "docs"},

	{Key: "theme", Section: "ui", Source: "schema"},
	{Key: "tui", Section: "ui", Source: "schema"},
	{Key: "prefersReducedMotion", Section: "ui", Source: "schema"},
	{Key: "spinnerTipsEnabled", Section: "ui", Source: "schema"},

	{Key: "agentPushNotifEnabled", Section: "notifications", Source: "schema"},
	{Key: "inputNeededNotifEnabled", Section: "notifications", Source: "schema"},
	{Key: "preferredNotifChannel", Section: "notifications", Source: "schema"},
	{Key: "awaySummaryEnabled", Section: "notifications", Source: "schema"},
	{Key: "feedbackDrafts", Section: "notifications", Kind: KindEnum, Values: []string{"notify", "quiet", "off"}, Source: "docs"},

	{Key: "cleanupPeriodDays", Section: "session", Source: "schema"},
	{Key: "autoConnectIde", Section: "session", Source: "schema"},
	{Key: "disableClaudeAiConnectors", Section: "session", Source: "schema"},
	{Key: "useAutoModeDuringPlan", Section: "session", Source: "schema"},
	{Key: "skipWorkflowUsageWarning", Section: "session", Kind: KindBool, Source: "docs"},

	// Section "" is overlay-only: set via `claude-code vault edit`, never shown on the configure page.
	{Key: "autoMode", Section: "", Source: "schema"},
}

// resolveSchema fills Kind, Values and Min from the schema for every entry that is not docs-sourced.
func resolveSchema(spec []Field) []Field {
	out := make([]Field, len(spec))
	for i, f := range spec {
		if k, ok := SchemaKey(f.Key); ok && f.Source != "docs" {
			f.Kind, f.Values = k.Kind, k.Enum
			if k.Min != nil {
				f.Min = int(*k.Min)
			}
		}
		out[i] = f
	}
	return out
}

var catalogIndex = buildCatalogIndex()

func buildCatalogIndex() map[string]Field {
	m := make(map[string]Field, len(Catalog))
	for _, f := range Catalog {
		m[f.Key] = f
	}
	return m
}

// Lookup returns key's catalog entry.
func Lookup(key string) (Field, bool) {
	f, ok := catalogIndex[key]
	return f, ok
}

// Parse turns user-typed text into a validated JSON value for this field.
func (f Field) Parse(input string) (json.RawMessage, error) {
	input = strings.TrimSpace(input)
	var raw json.RawMessage
	switch f.Kind {
	case KindBool:
		switch strings.ToLower(input) {
		case "true":
			raw = json.RawMessage("true")
		case "false":
			raw = json.RawMessage("false")
		default:
			return nil, fmt.Errorf("%s: want true or false, got %q", f.Key, input)
		}
	case KindInt:
		n, err := strconv.Atoi(input)
		if err != nil {
			return nil, fmt.Errorf("%s: want an integer, got %q", f.Key, input)
		}
		raw = json.RawMessage(strconv.Itoa(n))
	case KindEnum, KindString:
		b, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		raw = b
	case KindObject:
		return nil, fmt.Errorf("%s: edit with claude-code vault edit", f.Key)
	default:
		return nil, fmt.Errorf("%s: unknown kind", f.Key)
	}
	if err := f.Validate(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// Validate checks raw against this field's kind, enum values, or minimum. theme additionally
// accepts "custom:<non-empty>" for a user theme defined under ~/.claude/themes/.
func (f Field) Validate(raw json.RawMessage) error {
	if strings.TrimSpace(string(raw)) == "null" {
		return fmt.Errorf("%s: want a value, got null", f.Key)
	}
	switch f.Kind {
	case KindBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("%s: want a boolean", f.Key)
		}
	case KindInt:
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return fmt.Errorf("%s: want an integer", f.Key)
		}
		if n < f.Min {
			return fmt.Errorf("%s: must be >= %d, got %d", f.Key, f.Min, n)
		}
	case KindEnum:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("%s: want a string", f.Key)
		}
		if f.Key == "theme" && strings.HasPrefix(s, "custom:") && s != "custom:" {
			return nil
		}
		if !slices.Contains(f.Values, s) {
			return fmt.Errorf("%s: want one of %v, got %q", f.Key, f.Values, s)
		}
	case KindString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("%s: want a string", f.Key)
		}
	case KindObject:
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("%s: want a JSON object", f.Key)
		}
	default:
		return fmt.Errorf("%s: unknown kind", f.Key)
	}
	return nil
}
