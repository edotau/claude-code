package proxy

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// Every non-empty router suffix an agent row declares lands on a wire the passthrough parser accepts, and the empty one
// (anthropic) stays off it.
func TestAgentSpecRouterSuffixesParse(t *testing.T) {
	seen := 0
	for _, spec := range providers.AgentSpecs() {
		for wire, suffix := range spec.RouterSuffix {
			base := SessionBase("http://127.0.0.1:1", "prov") + suffix
			path := base[len("http://127.0.0.1:1"):] + "/v1/x"
			prov, dialect, rest, ok := parsePassthrough(path)
			if suffix == "" {
				if ok {
					t.Errorf("%s/%s: empty suffix parsed as passthrough %q", spec.Name, wire, path)
				}
				continue
			}
			seen++
			if !ok || prov != "prov" || dialect != wire || rest != "v1/x" {
				t.Errorf("%s/%s: %q -> (%q, %q, %q, %v), want dialect %q", spec.Name, wire, path, prov, dialect, rest, ok, wire)
			}
		}
		for _, d := range spec.Dialects {
			if _, ok := spec.RouterSuffix[d]; !ok {
				t.Errorf("%s: dialect %q has no router suffix", spec.Name, d)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no router suffix checked")
	}
}
