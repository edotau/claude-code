package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sealedKinds are the sealed authTypes whose mint goes through the OIDC token endpoint.
var sealedKinds = []struct{ kind, clientID, secret string }{
	{AuthOAuth, "", "refresh-1"},
	{AuthM2M, "sp-client", "sp-secret-xyz"},
}

func mintFollowersReady(t *testing.T, n int32) <-chan struct{} {
	t.Helper()
	var followers atomic.Int32
	ready := make(chan struct{})
	setFlightHook(t, func(leader bool) {
		if !leader && followers.Add(1) == n {
			close(ready)
		}
	})
	return ready
}

func TestSealedAuthHerdMintsOnce(t *testing.T) {
	for _, k := range sealedKinds {
		t.Run(k.kind, func(t *testing.T) {
			var posts atomic.Int32
			ready := mintFollowersReady(t, 19) // all 19 followers of the 20-caller herd must wait on the flight
			srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
				posts.Add(1)
				select { // hold the flight open until every follower is waiting on it
				case <-ready:
				case <-time.After(10 * time.Second):
					t.Error("not all 19 followers joined the mint flight")
				}
				return 200, map[string]any{"access_token": "herd-token", "expires_in": 3600}
			})
			p := mintRow(t, "sf-herd-"+k.kind, k.kind, k.clientID, srv.URL)
			sealFor(t, p, k.secret)
			vals, errs := herd(p, 20)
			for i := range vals {
				if errs[i] != nil || vals[i] != "herd-token" {
					t.Fatalf("caller %d: %d-byte token, %v", i, len(vals[i]), errs[i])
				}
			}
			if n := posts.Load(); n != 1 {
				t.Fatalf("token endpoint hit %d times, want 1", n)
			}
		})
	}
}

func TestSealedAuthRefreshDuringInflightMintRunsForcedMint(t *testing.T) {
	for _, k := range sealedKinds {
		t.Run(k.kind, func(t *testing.T) {
			var posts atomic.Int32
			gate := make(chan struct{})
			srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
				n := posts.Add(1)
				if n == 1 {
					<-gate // the non-forced mint stays in flight until released
				}
				return 200, map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "expires_in": 3600}
			})
			p := mintRow(t, "sf-refresh-"+k.kind, k.kind, k.clientID, srv.URL)
			sealFor(t, p, k.secret)
			var wg sync.WaitGroup
			wg.Go(func() { _, _ = Credential(context.Background(), p) })
			deadline := time.Now().Add(5 * time.Second)
			for posts.Load() == 0 {
				if time.Now().After(deadline) {
					close(gate)
					t.Fatal("non-forced mint never reached the endpoint")
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.AfterFunc(150*time.Millisecond, func() { close(gate) }) // Refresh is queued behind the flight by then
			got, err := Refresh(context.Background(), p)
			wg.Wait()
			if err != nil || got != "tok-2" {
				t.Fatalf("Refresh = %d-byte token, %v; want the forced mint tok-2, not the in-flight result", len(got), err)
			}
			if n := posts.Load(); n != 2 {
				t.Fatalf("token endpoint hit %d times, want 2 (one non-forced, one forced)", n)
			}
		})
	}
}

const nestedTestDoc = `{"default":"work","providers":{
 "anthropic":{"work":{"protocol":"anthropic","base_url":"https://api.anthropic.com","auth":{"type":"key"}}},
 "openai":{"scratch":{"protocol":"openai","base_url":"https://api.openai.com/v1","auth":{"type":"key"}}}}}`

func TestParseNestedByProvider(t *testing.T) {
	reg, err := Parse(defaultsJSON, []byte(nestedTestDoc))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"work": "anthropic", "scratch": "openai", "max": "anthropic", "personal": "github"} {
		if p := reg.Providers[name]; p == nil || p.Provider != want || p.Name != name {
			t.Errorf("profile %q = %+v, want provider %q", name, p, want)
		}
	}
	if reg.Default != "work" {
		t.Errorf("default = %q", reg.Default)
	}
	if got := reg.ProfilesOf("anthropic"); strings.Join(got, ",") != "max,anthropic,work" {
		t.Errorf("ProfilesOf(anthropic) = %v", got)
	}
}

func TestBuiltinProfileNames(t *testing.T) {
	reg, err := Parse(defaultsJSON, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"max": "anthropic", "anthropic": "anthropic", "personal": "github", "chatgpt": "openai",
		"openai": "openai", "antigravity": "google"}
	if len(reg.Providers) != len(want) {
		t.Errorf("built-ins = %v, want exactly %d profiles (no ollama)", reg.Names(), len(want))
	}
	for n, prov := range want {
		if p := reg.Providers[n]; p == nil || p.Provider != prov {
			t.Errorf("built-in %q = %+v, want provider %q", n, p, prov)
		}
	}
}

func TestDuplicateProfileRejected(t *testing.T) {
	cases := map[string]string{
		"two providers in one doc": `{"providers":{"anthropic":{"dup":{"protocol":"anthropic","base_url":"https://x","auth":{"type":"none"}}},` +
			`"openai":{"dup":{"protocol":"openai","base_url":"https://y","auth":{"type":"none"}}}}}`,
		"overlay profile owned by another provider": `{"providers":{"openai":{"max":{"protocol":"openai","base_url":"https://y","auth":{"type":"none"}}}}}`,
	}
	for name, doc := range cases {
		_, err := Parse(defaultsJSON, []byte(doc))
		if err == nil || !strings.Contains(err.Error(), "unique across providers") {
			t.Errorf("%s: err = %v, want a unique-across-providers rejection", name, err)
		}
	}
}

func TestOverlayWritesNestedAndAcceptsFlatView(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := SetProviderFields("qa", map[string]any{
		"preset": "databricks", "host": "https://dbc-3.cloud.databricks.com", "cliProfile": "qa",
	}); err != nil {
		t.Fatal(err)
	}
	doc := readOverlay(t, "providers")
	if !strings.Contains(doc, `"databricks": {`) || !strings.Contains(doc, `"qa": {`) {
		t.Fatalf("overlay is not nested by provider:\n%s", doc)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reg.Providers["qa"].Provider != "databricks" {
		t.Errorf("qa provider = %q", reg.Providers["qa"].Provider)
	}
	if got := DatabricksProfiles(reg); len(got) != 1 || got[0] != "qa" {
		t.Errorf("DatabricksProfiles = %v", got)
	}
}

func TestWireGatewaySpellingRead(t *testing.T) {
	for _, w := range []string{"ai-gateway"} {
		doc := `{"providers":{"databricks":{"gw":{"preset":"databricks","host":"https://h.cloud.databricks.com","options":{"wire":"` + w + `"}}}}}`
		reg, err := Parse(defaultsJSON, []byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		p := reg.Providers["gw"]
		if p.Options.Wire != WireAIGateway || p.WireModel("claude-opus-5-5") != "system.ai.claude-opus-5-5" {
			t.Errorf("wire %q: %q, %q", w, p.Options.Wire, p.WireModel("claude-opus-5-5"))
		}
	}
}

func TestRoutingCellsAcceptClaudeIDsAndAliases(t *testing.T) {
	for key, ok := range map[string]bool{"opus": true, "claude-opus-5-5": true, "claude-haiku-5-5": true, "claude-fable-5-1": false, "bogus": false} {
		doc := `{"routing":{"cells":{"claude":{"` + key + `":{"model":"claude-opus-5-5"}}}}}`
		_, err := Parse(defaultsJSON, []byte(doc))
		if (err == nil) != ok {
			t.Errorf("cells key %q: err = %v, want accepted=%v", key, err, ok)
		}
	}
}

func TestDuplicateOpencodeIDRejected(t *testing.T) {
	doc := `{"providers":{"openai":{"openai":{"protocol":"openai","base_url":"https://api.openai.com/v1","auth":{"type":"key"},"opencodeID":"openai"}}}}`
	if _, err := Parse(defaultsJSON, []byte(doc)); err == nil || !strings.Contains(err.Error(), "both claim opencodeID") {
		t.Errorf("err = %v, want an opencodeID ownership rejection", err)
	}
	reg, _ := Parse(defaultsJSON, nil)
	if reg.Providers["chatgpt"].OpencodeID != "openai" || reg.Providers["openai"].OpencodeID != "" {
		t.Error("chatgpt must own opencodeID openai in the built-ins")
	}
}

func TestSameModelIgnoresOneMAndSpelling(t *testing.T) {
	p := Provider{Models: map[string]string{"opus": "claude-opus-5-5[1m]", "sonnet": "databricks-claude-sonnet-5-5"}}
	if got := p.AliasOf("claude-opus-5-5"); got != "opus" {
		t.Errorf("AliasOf(plain) with a [1m] table = %q", got)
	}
	if got := p.AliasOf("claude-sonnet-5.5"); got != "sonnet" {
		t.Errorf("AliasOf(dotted) = %q", got)
	}
	if tgt, ok := Demote(&p, "claude-opus-5-5"); !ok || tgt.Model != "databricks-claude-sonnet-5-5" {
		t.Errorf("Demote = %+v, %v", tgt, ok)
	}
}

func TestWireModelNoDoublePrefix(t *testing.T) {
	p := Provider{Preset: PresetDatabricks, Options: Options{Wire: WireAIGateway}}
	for _, in := range []string{"system.ai.claude-opus-5-5", "databricks-claude-opus-5-5", "claude-opus-5.5", "opus"} {
		if got := p.WireModel(in); got != "system.ai.claude-opus-5-5" {
			t.Errorf("WireModel(%q) = %q", in, got)
		}
	}
}

func TestProfilesOfFollowsDeclaredOrder(t *testing.T) {
	reg := mustParse(t, `{"providers":{"databricks":{
"dev":{"preset":"databricks","host":"https://h.cloud.databricks.com"},
"qa":{"preset":"databricks","host":"https://h.cloud.databricks.com"},
"prod":{"preset":"databricks","host":"https://h.cloud.databricks.com"}}}}`)
	if got := strings.Join(DatabricksProfiles(reg), ","); got != "dev,qa,prod" {
		t.Errorf("DatabricksProfiles = %s, want dev,qa,prod", got)
	}
}

// A malformed overlay still yields the base registry named, ordered and preset-expanded, alongside the error.
func TestParseBadOverlayKeepsBuiltBase(t *testing.T) {
	reg, err := Parse(defaultsJSON, []byte(`{"providers": `))
	if err == nil {
		t.Fatal("want an error for a malformed overlay")
	}
	if reg == nil || len(reg.Providers) == 0 {
		t.Fatalf("registry = %+v, want the built base", reg)
	}
	for n, p := range reg.Providers {
		if p.Name != n {
			t.Fatalf("provider %q has Name %q", n, p.Name)
		}
	}
	if len(reg.order) == 0 {
		t.Error("declared order is empty")
	}
}
