package providers

import (
	"strings"
	"testing"
)

func TestAuthMethodPerType(t *testing.T) {
	for _, typ := range []string{AuthOAuth, AuthM2M, AuthPAT, AuthKey, AuthCLI, AuthNone} {
		a := Auth{Type: typ}
		if a.Method() != typ {
			t.Errorf("Method() for %q = %q", typ, a.Method())
		}
		wantSealed := typ == AuthOAuth || typ == AuthM2M || typ == AuthPAT
		if a.Sealed() != wantSealed {
			t.Errorf("Sealed() for %q = %v, want %v", typ, a.Sealed(), wantSealed)
		}
		wantHeader := ""
		if wantSealed || typ == AuthKey {
			wantHeader = HeaderBearer
		}
		if a.HeaderStyle() != wantHeader {
			t.Errorf("HeaderStyle() for %q = %q, want %q", typ, a.HeaderStyle(), wantHeader)
		}
	}
	if (Auth{}).Method() != "" {
		t.Error("an unset Auth must report an empty method")
	}
	if got := (Auth{Type: AuthKey, Header: HeaderXAPIKey}).HeaderStyle(); got != HeaderXAPIKey {
		t.Errorf("explicit header = %q", got)
	}
}

func TestRetiredAuthSpellingsAreRejected(t *testing.T) {
	for _, typ := range []string{"bearer", "u2m", "oauth-m2m", "api-key", "passthrough"} {
		doc := `{"providers":{"anthropic":{"x":{"protocol":"anthropic","base_url":"https://x.example","auth":{"type":"` + typ + `"}}}}}`
		if _, err := parseOverlay(t, doc); err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Errorf("retired auth type %q: err = %v, want an unknown-type error", typ, err)
		}
	}
}

func TestFlatOverlayIsNotReadByTheRuntime(t *testing.T) {
	flat := `{"providers":{"gw":{"protocol":"anthropic","base_url":"https://gw.example","auth":{"type":"none"}}}}`
	reg, err := parseOverlay(t, flat)
	if err == nil {
		if reg.Providers["gw"] != nil {
			t.Fatal("flat overlay row gw was read")
		}
		return
	}
	if !strings.Contains(err.Error(), `unknown provider "gw"`) {
		t.Errorf("err = %v, want it to name the unknown provider", err)
	}
}

func TestOneMIsIgnoredOnLoad(t *testing.T) {
	doc := `{"providers":{"anthropic":{"x":{"protocol":"anthropic","base_url":"https://x.example","auth":{"type":"none"},"one_m":true}}}}`
	reg, err := parseOverlay(t, doc)
	if err != nil {
		t.Fatalf("one_m must not break loading: %v", err)
	}
	if p := reg.Providers["x"]; p == nil || p.Auth.Method() != AuthNone {
		t.Errorf("row x = %+v", p)
	}
}

func TestLegacyPinNamesAreNotRead(t *testing.T) {
	reg := load(t)
	writeProviderPins(t, map[string]string{"HARNESS_PROVIDER": "dev", "HARNESS_MODEL": "legacy-model", "COPILOT_GITHUB_USER": "someone"})
	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider.Name == "dev" {
		t.Error("legacy HARNESS_PROVIDER pin selected a profile")
	}
	for slot, tg := range sel.Slots {
		if tg.Model == "legacy-model" {
			t.Errorf("slot %s took the legacy HARNESS_MODEL pin", slot)
		}
	}
	if got, _ := CopilotLogin(reg); got == "someone" {
		t.Error("legacy COPILOT_GITHUB_USER pin was read as a login")
	}
}
