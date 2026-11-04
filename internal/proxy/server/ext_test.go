package server_test

import (
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/codex" // register the rows whose suffixes are checked
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxy/server"
)

// Every registered agent row's router suffix parses back to its provider and wire; an empty suffix is not a passthrough.
func TestRegisteredRowRouterSuffixesRoundTrip(t *testing.T) {
	seen := 0
	for _, spec := range agent.Roster() {
		for wire, suffix := range spec.CLI.RouterSuffix {
			if checkRouterSuffix(t, spec.Name, wire, suffix) {
				seen++
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

// checkRouterSuffix verifies one row/wire suffix; true when a non-empty suffix was checked.
func checkRouterSuffix(t *testing.T, name, wire, suffix string) bool {
	t.Helper()
	base := proxy.SessionBase("http://127.0.0.1:1", "prov") + suffix
	path := base[len("http://127.0.0.1:1"):] + "/v1/x"
	prov, dialect, rest, ok := server.ParsePassthrough(path)
	if suffix == "" {
		if ok {
			t.Errorf("%s/%s: empty suffix parsed as passthrough %q", name, wire, path)
		}
		return false
	}
	if !ok || prov != "prov" || dialect != wire || rest != "v1/x" {
		t.Errorf("%s/%s: %q -> (%q, %q, %q, %v), want dialect %q", name, wire, path, prov, dialect, rest, ok, wire)
	}
	return true
}
