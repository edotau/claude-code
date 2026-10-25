package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	gwProv   = `"gw":{"kind":"anthropic","base_url":"https://gw.example/anthropic","auth":{"type":"bearer","env":"TEST_GW_KEY"},"models":{"opus":"claude-opus-4-6","sonnet":"claude-sonnet-4-6"},"env":{"GW_REGION":"eu","GW_TIER":"x"}}`
	ptProv   = `"pt":{"kind":"anthropic","base_url":"https://pt.example/anthropic","auth":{"type":"passthrough"},"models":{"opus":"claude-opus-4-6"},"env":{"GW_REGION":"eu"}}`
	plainGw  = `"plain":{"kind":"anthropic","base_url":"https://plain.example/anthropic","auth":{"type":"bearer","env":"TEST_GW_KEY"},"models":{"opus":"claude-opus-4-6"}}`
	noModels = `"fallback":[]`
)

func TestProviderEnvLandsInPlanSet(t *testing.T) {
	hermetic(t, `{`+noModels+`,"providers":{`+gwProv+`,`+ptProv+`}}`)
	t.Setenv("GW_REGION", "")
	t.Setenv("GW_TIER", "")
	for _, prov := range []string{"gw", "pt"} {
		p, err := Claude(context.Background(), Options{Provider: prov}, []string{"-p", "hi"})
		if err != nil {
			t.Fatalf("%s: %v", prov, err)
		}
		if p.Set["GW_REGION"] != "eu" {
			t.Errorf("%s: Set[GW_REGION] = %q, want eu", prov, p.Set["GW_REGION"])
		}
		if strings.Contains(string(p.Overlay), "GW_REGION") {
			t.Errorf("%s: provider env must not enter the settings overlay: %s", prov, p.Overlay)
		}
	}
}

func TestProviderEnvSkipsProcessAndCustomEnv(t *testing.T) {
	hermetic(t, `{`+noModels+`,"providers":{`+gwProv+`}}`)
	t.Setenv("GW_REGION", "from-process")
	t.Setenv("GW_TIER", "")
	old := customEnv
	customEnv = func() map[string]string { return map[string]string{"GW_TIER": "from-vault"} }
	t.Cleanup(func() { customEnv = old })
	p, err := Claude(context.Background(), Options{Provider: "gw"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"GW_REGION", "GW_TIER"} {
		if _, ok := p.Set[k]; ok {
			t.Errorf("Set[%s] = %q, want it skipped (already set elsewhere)", k, p.Set[k])
		}
	}
}

func TestNilProviderEnvLeavesOverlayExact(t *testing.T) {
	hermetic(t, `{`+noModels+`,"providers":{`+plainGw+`}}`)
	p, err := Claude(context.Background(), Options{Provider: "plain"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(p.Overlay, &doc); err != nil || len(doc) != 1 || doc["apiKeyHelper"] == nil {
		t.Fatalf("overlay = %s, want exactly {apiKeyHelper}", p.Overlay)
	}
	for k := range p.Set {
		if strings.HasPrefix(k, "GW_") {
			t.Errorf("unexpected provider env %s in Set", k)
		}
	}
}

const profileDoc = `{` + noModels + `,"providers":{` + gwProv + `,` + plainGw + `},"profiles":{"active":"%s","sets":{"w":{"session":%s}}}}`

func profileFixtureLaunch(t *testing.T, active, row string) {
	t.Helper()
	hermetic(t, fmt.Sprintf(profileDoc, active, row))
	t.Setenv("GW_REGION", "")
	t.Setenv("GW_TIER", "")
}

func TestSessionRowPrecedence(t *testing.T) {
	cases := []struct {
		name, active, row string
		opts              Options
		wantBase          string
		wantOpus          string
		wantSonnet        string
		wantProfile       string
	}{
		{"row account and Model->opus", "w", `{"account":"gw","model":"row-opus"}`, Options{},
			"https://gw.example/anthropic", "row-opus", "claude-sonnet-4-6", "w"},
		{"row Models override slots", "w", `{"account":"gw","models":{"opus":"m-opus","sonnet":"m-son"}}`, Options{},
			"https://gw.example/anthropic", "m-opus", "m-son", "w"},
		{"--model beats row", "w", `{"account":"gw","model":"row-opus"}`, Options{Model: "cli-opus"},
			"https://gw.example/anthropic", "cli-opus", "claude-sonnet-4-6", "w"},
		{"--provider beats row", "w", `{"account":"gw","model":"row-opus"}`, Options{Provider: "plain"},
			"https://plain.example/anthropic", "claude-opus-4-6", "", ""},
		{"no active profile unchanged", "", `{"account":"gw","model":"row-opus"}`, Options{Provider: "plain"},
			"https://plain.example/anthropic", "claude-opus-4-6", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			profileFixtureLaunch(t, c.active, c.row)
			p, err := Claude(context.Background(), c.opts, nil)
			if err != nil {
				t.Fatal(err)
			}
			if p.Set["ANTHROPIC_BASE_URL"] != c.wantBase || p.Set["ANTHROPIC_DEFAULT_OPUS_MODEL"] != c.wantOpus ||
				p.Set["ANTHROPIC_DEFAULT_SONNET_MODEL"] != c.wantSonnet || p.Profile != c.wantProfile {
				t.Errorf("base %q opus %q sonnet %q profile %q; want %q %q %q %q", p.Set["ANTHROPIC_BASE_URL"],
					p.Set["ANTHROPIC_DEFAULT_OPUS_MODEL"], p.Set["ANTHROPIC_DEFAULT_SONNET_MODEL"], p.Profile,
					c.wantBase, c.wantOpus, c.wantSonnet, c.wantProfile)
			}
		})
	}
}

func TestVendorSelectionIgnoresSessionRow(t *testing.T) {
	profileFixtureLaunch(t, "w", `{"account":"gw","model":"row-opus"}`)
	_, sel, err := selection(Options{Provider: "plain"})
	if err != nil || sel.Provider == nil || sel.Provider.Name != "plain" {
		t.Fatalf("selection = %+v, %v", sel.Provider, err)
	}
	_, sel, _, err = sessionSelection(Options{}, false)
	if err == nil && sel.Provider != nil && sel.Provider.Name == "gw" {
		t.Error("session=false must not apply the session row")
	}
}

func TestPrintProfileLine(t *testing.T) {
	var b bytes.Buffer
	Plan{Profile: "w"}.Print(&b)
	if !strings.Contains(b.String(), "profile  w (session row applied)\n") {
		t.Errorf("Print = %q", b.String())
	}
	b.Reset()
	Plan{}.Print(&b)
	if strings.Contains(b.String(), "profile ") {
		t.Errorf("no profile: Print = %q", b.String())
	}
}
