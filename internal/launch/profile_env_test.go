package launch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const (
	gwProv   = `"gw":{"protocol":"anthropic","base_url":"https://gw.example/anthropic","auth":{"type":"key","env":"TEST_GW_KEY"},"models":{"opus":"claude-opus-4-6","sonnet":"claude-sonnet-4-6"},"env":{"GW_REGION":"eu","GW_TIER":"x"}}`
	ptProv   = `"pt":{"protocol":"anthropic","base_url":"https://pt.example/anthropic","auth":{"type":"cli"},"models":{"opus":"claude-opus-4-6"},"env":{"GW_REGION":"eu"}}`
	plainGw  = `"plain":{"protocol":"anthropic","base_url":"https://plain.example/anthropic","auth":{"type":"key","env":"TEST_GW_KEY"},"models":{"opus":"claude-opus-4-6"}}`
	noModels = `"fallback":[]`
)

func TestProviderEnvLandsInPlanSet(t *testing.T) {
	hermetic(t, `{`+noModels+`,"providers":{"anthropic":{`+gwProv+`,`+ptProv+`}}}`)
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
	hermetic(t, `{`+noModels+`,"providers":{"anthropic":{`+gwProv+`}}}`)
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
	hermetic(t, `{`+noModels+`,"providers":{"anthropic":{`+plainGw+`}}}`)
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

const profileDoc = `{` + noModels + `,"providers":{"anthropic":{` + gwProv + `,` + plainGw + `}}}`

func profileFixtureLaunch(t *testing.T) {
	t.Helper()
	hermetic(t, profileDoc)
	t.Setenv("GW_REGION", "")
	t.Setenv("GW_TIER", "")
}

func TestSessionFlagPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		opts     Options
		wantBase string
		wantOpus string
	}{
		{"--profile selects the base", Options{Provider: "plain"}, "https://plain.example/anthropic", "claude-opus-4-6"},
		{"--model beats the default", Options{Provider: "plain", Model: "cli-opus"}, "https://plain.example/anthropic", "cli-opus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			profileFixtureLaunch(t)
			p, err := Claude(context.Background(), c.opts, nil)
			if err != nil {
				t.Fatal(err)
			}
			if p.Set["ANTHROPIC_BASE_URL"] != c.wantBase || p.Set["ANTHROPIC_DEFAULT_OPUS_MODEL"] != c.wantOpus {
				t.Errorf("base %q opus %q; want %q %q", p.Set["ANTHROPIC_BASE_URL"], p.Set["ANTHROPIC_DEFAULT_OPUS_MODEL"],
					c.wantBase, c.wantOpus)
			}
		})
	}
}
