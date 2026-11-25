package providers

import (
	"strings"
	"testing"
)

func lastResortRow(lr bool, conc int, anth string) *Provider {
	p := &Provider{Protocol: ProtocolOpenAI, BaseURL: "http://127.0.0.1:10100/v1", Concurrency: conc,
		Auth: Auth{Type: AuthKey, Command: "echo x"}, Options: Options{LastResort: lr}}
	if anth != "" {
		p.Routes = map[string]string{"anthropic": anth}
	}
	return p
}

func TestLastResort(t *testing.T) {
	reg := &Registry{Providers: map[string]*Provider{
		"b": lastResortRow(true, 1, "http://127.0.0.1:10100/a"), "a": lastResortRow(false, 0, "")}}
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

func TestLastResortOneRow(t *testing.T) {
	reg := &Registry{Providers: map[string]*Provider{
		"b": lastResortRow(true, 1, "http://127.0.0.1:10100/a"), "c": lastResortRow(true, 1, "http://127.0.0.1:10100/a")}}
	errs := validateOneLastResort(reg)
	if len(errs) != 1 || !strings.Contains(errs[0], "b") || !strings.Contains(errs[0], "c") {
		t.Fatalf("two rows: errs = %v, want one error naming b and c", errs)
	}
	if err := reg.validate(); err == nil || !strings.Contains(err.Error(), "last_resort") {
		t.Fatalf("registry validate = %v, want a last_resort error", err)
	}
	delete(reg.Providers, "c")
	if errs := validateOneLastResort(reg); len(errs) != 0 {
		t.Fatalf("one row refused: %v", errs)
	}
}

func TestLastResortLoopbackURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		good bool
	}{
		{"http://127.0.0.1:10100/inference/anthropic", true},
		{"http://localhost:10100/x", true},
		{"http://[::1]:10100/x", true},
		{"https://remote.example", false},
		{"http://user:pass@127.0.0.1:10100", false},
		{"http://127.0.0.1:10100/x?key=1", false},
		{"http://127.0.0.1:10100/x#f", false},
		{"httpx://127.0.0.1:10100", false},
		{"http://127.0.0.2:10100", false},
		{"http://127.0.0.1.evil.example/x", false},
		{"127.0.0.1:10100", false},
	} {
		for _, row := range []*Provider{lastResortRow(true, 1, tc.url), lastResortRow(true, 1, "http://127.0.0.1:1/a")} {
			if row.Routes["anthropic"] != tc.url {
				row.BaseURL = tc.url
			}
			errs := validateLastResort("x", row)
			if tc.good != (len(errs) == 0) {
				t.Errorf("url %q: errs = %v, want good=%v", tc.url, errs, tc.good)
			}
		}
	}
}
