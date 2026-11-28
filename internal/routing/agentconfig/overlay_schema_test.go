package agentconfig

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/router"
)

type obj = map[string]any

func (k fieldKind) base() obj {
	switch k {
	case kindList:
		return obj{"type": "array", "items": obj{"type": "string"}}
	case kindStrings:
		return obj{"type": "object", "additionalProperties": obj{"type": "string"}}
	case kindFence:
		return obj{"type": "object", "additionalProperties": false, "properties": obj{
			"profiles": kindList.base(), "open": kindBool.base()}}
	}
	return obj{"type": string(k)}
}

// settingLimits are the constraints on the keys outside routeKeys; keyType words the two numeric ones.
var settingLimits = map[string]obj{
	"schema":        {"minimum": 0, "maximum": overlaySchema},
	"tokenCommand":  {"minItems": 1},
	"codemode":      {"enum": []string{"off", "on", "only"}},
	"budgetUSD":     {"exclusiveMinimum": 0},
	"budgetCredits": {"minimum": 30},
	"tier":          {"enum": []string{"fast", "standard", "deep"}},
	"timeout":       {"pattern": durationPattern, "description": "Go duration, e.g. 9m30s"},
	"googleTools":   {"pattern": googleToolsPattern()},
	"providerMode":  {"additionalProperties": obj{"enum": router.ProviderModes}},
}

// routeKeyEnums are the routeKeys whose value is a closed list; the lists come from the validators' own sources.
func routeKeyEnums() map[string][]string {
	return map[string][]string{
		"provider": providers.ProviderKeys(), "effort": agentid.EffortNames(),
		"fallback": route.FallbackModes, "identity": identityModes, "runner": route.Runners,
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

func TestOverlaySchemaGolden(t *testing.T) {
	got := OverlaySchema()
	if bytes.Equal(got, assets.OverlaySchemaSnapshot()) {
		return
	}
	if os.Getenv("UPDATE_OVERLAY_SCHEMA") != "" {
		if err := os.WriteFile("../../assets/agentconfig/overlay.schema.json", got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("internal/assets/agentconfig/overlay.schema.json is stale: regenerate with `UPDATE_OVERLAY_SCHEMA=1 go test ./internal/agentconfig -run TestOverlaySchemaGolden`")
}

func TestOverlaySchemaCoversEveryKey(t *testing.T) {
	var doc struct{ Properties map[string]any }
	if err := json.Unmarshal(OverlaySchema(), &doc); err != nil {
		t.Fatal(err)
	}
	names := overlayNames()
	if len(doc.Properties) != len(names) {
		t.Errorf("properties %d, struct keys %d", len(doc.Properties), len(names))
	}
	for _, k := range names {
		if doc.Properties[k] == nil {
			t.Errorf("key %s missing", k)
		}
	}
}

func TestRouteProvidersMatchRegistry(t *testing.T) {
	if !slices.Equal(route.Providers, providers.ProviderKeys()) {
		t.Fatalf("route.Providers %v != %v", route.Providers, providers.ProviderKeys())
	}
}
