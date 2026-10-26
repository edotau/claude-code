package launch

import (
	"context"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Claude() scrubs the cloud-routing and OTLP header knobs a parent shell may have set.
func TestClaudePlanUnsetsRoutingAndOTLPKeys(t *testing.T) {
	vaulttest.Use(t)
	hermetic(t, "")
	p, err := Claude(context.Background(), Options{Provider: "subscription"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_GATEWAY",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "CLAUDE_CODE_ANTHROPIC_MODEL", "OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS"} {
		if !slices.Contains(p.Unset, k) {
			t.Errorf("plan.Unset missing %s", k)
		}
	}
}
