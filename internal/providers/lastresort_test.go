package providers

import (
	"strings"
	"testing"
)

func lastResortRow(lr bool, conc int, anth string) *Provider {
	p := &Provider{Protocol: ProtocolOpenAI, BaseURL: "http://x/v1", Concurrency: conc,
		Auth: Auth{Type: AuthKey, Command: "echo x"}, Options: Options{LastResort: lr}}
	if anth != "" {
		p.Routes = map[string]string{"anthropic": anth}
	}
	return p
}

func TestLastResort(t *testing.T) {
	reg := &Registry{Providers: map[string]*Provider{
		"b": lastResortRow(true, 1, "http://x/a"), "a": lastResortRow(false, 0, ""), "c": lastResortRow(true, 1, "http://x/a")}}
	if got := LastResort(reg); got != reg.Providers["b"] {
		t.Fatalf("LastResort = %v, want row b", got)
	}
	if LastResort(&Registry{Providers: map[string]*Provider{"a": reg.Providers["a"]}}) != nil || LastResort(nil) != nil {
		t.Fatal("want nil with no marked row")
	}
	if errs := validateLastResort("b", reg.Providers["b"]); len(errs) != 0 {
		t.Fatalf("valid row refused: %v", errs)
	}
	bad := strings.Join(validateLastResort("x", lastResortRow(true, 2, "")), ";")
	if !strings.Contains(bad, "routes.anthropic") || !strings.Contains(bad, "concurrency 1") {
		t.Fatalf("bad row errors = %q", bad)
	}
	def, err := Parse(providerDefaults(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := LastResort(def); p == nil || p.Name != "local" {
		t.Fatalf("defaults last resort = %v, want local", p)
	}
}
