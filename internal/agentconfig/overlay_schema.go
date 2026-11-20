package agentconfig

import (
	_ "embed"
	"encoding/json"
	"maps"
	"reflect"
	"strings"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// OverlaySchemaJSON is the checked-in schema; TestOverlaySchemaGolden keeps it equal to OverlaySchema().
//
//go:embed overlay.schema.json
var OverlaySchemaJSON []byte

type obj = map[string]any

type fieldKind string

const (
	kindString  fieldKind = "string"
	kindBool    fieldKind = "boolean"
	kindInt     fieldKind = "integer"
	kindNumber  fieldKind = "number"
	kindList    fieldKind = "[]string"
	kindStrings fieldKind = "map[string]string"
)

type overlayField struct {
	name string
	kind fieldKind
}

// overlayFields is the Overlay struct's JSON keys in order, walked once; an unmapped field type panics at init.
var overlayFields = walkOverlay(reflect.TypeFor[Overlay]())

func walkOverlay(t reflect.Type) []overlayField {
	var out []overlayField
	for f := range t.Fields() {
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, walkOverlay(f.Type)...)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || name == "files" {
			continue
		}
		out = append(out, overlayField{name, kindOf(f.Type)})
	}
	return out
}

var scalarKinds = map[reflect.Kind]fieldKind{
	reflect.String: kindString, reflect.Bool: kindBool, reflect.Int: kindInt, reflect.Float64: kindNumber,
}

func kindOf(t reflect.Type) fieldKind {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if k, ok := scalarKinds[t.Kind()]; ok {
		return k
	}
	switch {
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.String:
		return kindList
	case t.Kind() == reflect.Map && t.Key().Kind() == reflect.String && t.Elem().Kind() == reflect.String:
		return kindStrings
	}
	panic("agentconfig: overlay field type " + t.String() + " has no schema kind")
}

// overlayNames lists the overlay's JSON keys in struct order.
func overlayNames() []string {
	out := make([]string, len(overlayFields))
	for i, f := range overlayFields {
		out[i] = f.name
	}
	return out
}

func (k fieldKind) base() obj {
	switch k {
	case kindList:
		return obj{"type": "array", "items": obj{"type": "string"}}
	case kindStrings:
		return obj{"type": "object", "additionalProperties": obj{"type": "string"}}
	}
	return obj{"type": string(k)}
}

// settingLimits are the constraints on the keys outside routeKeys; keyType words the two numeric ones.
var settingLimits = map[string]obj{
	"schema":        {"minimum": 0, "maximum": overlaySchema},
	"tokenCommand":  {"minItems": 1},
	"budgetUSD":     {"exclusiveMinimum": 0},
	"budgetCredits": {"minimum": 30},
}

// routeKeyEnums are the routeKeys whose value is a closed list; the lists come from the validators' own sources.
func routeKeyEnums() map[string][]string {
	return map[string][]string{
		"provider": providers.ProviderKeys(), "effort": agentid.EffortNames(),
		"fallback": router.FallbackModes, "identity": identityModes,
	}
}

var routeKeyNotes = map[string]obj{
	"profile": {"type": "string", "pattern": `^[^:\s]+$`, "description": "registry row or auto"},
	"model":   {"type": "string", "minLength": 1, "description": "plain id or alias; no <profile>: prefix"},
	"account": {"type": "string", "minLength": 1},
	"host":    {"type": "string"},
	"remote":  {"type": "string"},
}

// OverlaySchema generates the overlay's JSON Schema (draft 2020-12); the Go checks stay authoritative.
func OverlaySchema() []byte {
	props := obj{}
	for _, f := range overlayFields {
		p := f.kind.base()
		maps.Copy(p, settingLimits[f.name])
		props[f.name] = p
	}
	enums := routeKeyEnums()
	for _, k := range routeKeys {
		if e, ok := enums[k.name]; ok {
			props[k.name] = obj{"enum": e}
		} else {
			props[k.name] = routeKeyNotes[k.name]
		}
	}
	doc := obj{
		"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": "claude-code/overlay.schema.json",
		"title": "claude-code dispatch overlay", "type": "object", "additionalProperties": false,
		"properties": props,
		"dependentRequired": obj{
			"profile": []string{"provider"}, "identity": []string{"provider"}, "account": []string{"provider"},
			"provider": []string{"profile"},
		},
		"not": obj{"required": []string{"host", "remote"}},
		"allOf": []obj{
			onlyWith("identity", "databricks"),
			onlyWith("account", "github"),
		},
	}
	out, _ := json.MarshalIndent(doc, "", "  ")
	return append(out, '\n')
}

// onlyWith requires provider to be want whenever key is present.
func onlyWith(key, want string) obj {
	return obj{"if": obj{"required": []string{key}}, "then": obj{"properties": obj{"provider": obj{"const": want}}}}
}
