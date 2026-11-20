package agentconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
)

func TestOverlaySchemaGolden(t *testing.T) {
	got := OverlaySchema()
	if bytes.Equal(got, OverlaySchemaJSON) {
		return
	}
	if os.Getenv("UPDATE_OVERLAY_SCHEMA") != "" {
		if err := os.WriteFile("overlay.schema.json", got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("overlay.schema.json is stale: regenerate with `UPDATE_OVERLAY_SCHEMA=1 go test ./internal/agentconfig -run TestOverlaySchemaGolden`")
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
