package agentconfig

import (
	_ "embed"
	"encoding/json"
	"maps"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// OverlaySchemaJSON is the checked-in schema; TestOverlaySchemaGolden keeps it equal to OverlaySchema().
//
//go:embed overlay.schema.json
var OverlaySchemaJSON []byte

type obj = map[string]any

// settingKeys types the keys outside routeKeys (they are Overlay fields with no routing rule); keyType words the same.
var settingKeys = obj{
	"schema":        obj{"type": "integer", "minimum": 0, "maximum": overlaySchema},
	"set":           obj{"type": "object", "additionalProperties": obj{"type": "string"}},
	"unset":         obj{"type": "array", "items": obj{"type": "string"}},
	"keep":          obj{"type": "array", "items": obj{"type": "string"}},
	"tokenCommand":  obj{"type": "array", "items": obj{"type": "string"}, "minItems": 1},
	"maxChildren":   obj{"type": "integer"},
	"maxTreeTokens": obj{"type": "integer"},
	"budgetUSD":     obj{"type": "number", "exclusiveMinimum": 0},
	"budgetCredits": obj{"type": "integer", "minimum": 30},
	"codexNetwork":  obj{"type": "boolean"},
	"cloud":         obj{"type": "boolean"},
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
	maps.Copy(props, settingKeys)
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
