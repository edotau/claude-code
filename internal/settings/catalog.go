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
)

// Field describes one editable template key: its section, its value shape, and where its
// allowed values came from.
type Field struct {
	Key     string
	Section string
	Kind    Kind
	Values  []string // KindEnum only
	Min     int      // KindInt only
	Source  string   // "schema" | "docs" | "template"
}

// Catalog lists every plain-value template key except "model" and "$schema". Source "schema"
// means the schemastore copy in testdata/claude-code-settings.schema.json documents it; "docs"
// means code.claude.com documents it but schemastore is behind; "template" means neither does.
var Catalog = []Field{
	{Key: "alwaysThinkingEnabled", Section: "model", Kind: KindBool, Source: "schema"},
	{Key: "outputStyle", Section: "model", Kind: KindString, Source: "schema"},
	{Key: "switchModelsOnFlag", Section: "model", Kind: KindBool, Source: "docs"},

	{Key: "theme", Section: "ui", Kind: KindEnum, Values: []string{
		"auto", "dark", "light", "dark-daltonized", "light-daltonized", "dark-ansi", "light-ansi",
	}, Source: "schema"},
	{Key: "tui", Section: "ui", Kind: KindEnum, Values: []string{"fullscreen", "default"}, Source: "schema"},
	{Key: "prefersReducedMotion", Section: "ui", Kind: KindBool, Source: "schema"},
	{Key: "spinnerTipsEnabled", Section: "ui", Kind: KindBool, Source: "schema"},

	{Key: "agentPushNotifEnabled", Section: "notifications", Kind: KindBool, Source: "schema"},
	{Key: "inputNeededNotifEnabled", Section: "notifications", Kind: KindBool, Source: "schema"},
	{Key: "preferredNotifChannel", Section: "notifications", Kind: KindEnum, Values: []string{
		"auto", "terminal_bell", "iterm2", "iterm2_with_bell", "kitty", "ghostty", "notifications_disabled",
	}, Source: "schema"},
	{Key: "awaySummaryEnabled", Section: "notifications", Kind: KindBool, Source: "schema"},
	{Key: "feedbackDrafts", Section: "notifications", Kind: KindEnum, Values: []string{"notify", "quiet", "off"}, Source: "docs"},

	{Key: "dialogExpiry", Section: "session", Kind: KindEnum, Values: []string{"60s", "5m", "10m", "never"}, Source: "docs"},
	{Key: "idleCompaction", Section: "session", Kind: KindBool, Source: "docs"},
	{Key: "cleanupPeriodDays", Section: "session", Kind: KindInt, Min: 1, Source: "schema"},
	{Key: "remoteControlAtStartup", Section: "session", Kind: KindBool, Source: "schema"},
	{Key: "autoConnectIde", Section: "session", Kind: KindBool, Source: "schema"},
	{Key: "useAutoModeDuringPlan", Section: "session", Kind: KindBool, Source: "schema"},
	{Key: "skipWorkflowUsageWarning", Section: "session", Kind: KindBool, Source: "docs"},

	// Section "" is overlay-only: set via `claude-code vault edit`, never shown on the configure page.
	{Key: "autoMode", Section: "", Kind: KindObject, Source: "schema"},
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
