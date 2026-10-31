package proxy_test

import (
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proxy"
)

// Every non-empty router suffix an agent row declares lands on a wire the passthrough parser accepts, and the empty one
// (anthropic) stays off it.
func TestRosterRouterSuffixesParse(t *testing.T) {
	seen := 0
	for _, spec := range agent.Roster() {
		for wire, suffix := range spec.CLI.RouterSuffix {
			base := proxy.SessionBase("http://127.0.0.1:1", "prov") + suffix
			path := base[len("http://127.0.0.1:1"):] + "/v1/x"
			prov, dialect, rest, ok := proxy.ParsePassthrough(path)
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
		for _, d := range spec.CLI.Dialects {
			if _, ok := spec.CLI.RouterSuffix[d]; !ok {
				t.Errorf("%s: dialect %q has no router suffix", spec.Name, d)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no router suffix checked")
	}
}
