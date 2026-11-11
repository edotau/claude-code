package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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
	for name, want := range map[string]string{"work": "anthropic", "scratch": "openai", "ultra": "anthropic", "personal": "github"} {
		if p := reg.Providers[name]; p == nil || p.Provider != want || p.Name != name {
			t.Errorf("profile %q = %+v, want provider %q", name, p, want)
		}
	}
	if reg.Default != "work" {
		t.Errorf("default = %q", reg.Default)
	}
	if got := reg.ProfilesOf("anthropic"); strings.Join(got, ",") != "ultra,anthropic,work" {
		t.Errorf("ProfilesOf(anthropic) = %v", got)
	}
}

// The embedded defaults validate on their own, and every built-in row is named by its key under a known provider.
func TestEmbeddedDefaultsValid(t *testing.T) {
	reg, err := Parse(defaultsJSON, nil)
	if err != nil || len(reg.Providers) == 0 {
		t.Fatalf("Parse(defaults) = %v, %d rows", err, len(reg.Providers))
	}
	for n, p := range reg.Providers {
		if p.Name != n || !slices.Contains(providerKeys, p.Provider) || !slices.Contains(reg.ProfilesOf(p.Provider), n) {
			t.Errorf("built-in %q = name %q under provider %q, want its own key under a known provider", n, p.Name, p.Provider)
		}
	}
}

func TestDuplicateProfileRejected(t *testing.T) {
	cases := map[string]string{
		"two providers in one doc": `{"providers":{"anthropic":{"dup":{"protocol":"anthropic","base_url":"https://x","auth":{"type":"none"}}},` +
			`"openai":{"dup":{"protocol":"openai","base_url":"https://y","auth":{"type":"none"}}}}}`,
		"overlay profile owned by another provider": `{"providers":{"openai":{"ultra":{"protocol":"openai","base_url":"https://y","auth":{"type":"none"}}}}}`,
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

func TestLoadDecoratesPresetModels(t *testing.T) {
	reg := mustParse(t, `{"providers":{"databricks":{"dbx":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net/","cliProfile":"work",
	  "models":{"opus":"claude-opus-5-5[1m]","sonnet":"gpt-5.5"}}}}}`)
	m := reg.Providers["dbx"].Models
	want := map[string]string{
		"opus": "databricks-claude-opus-5-5[1m]", "sonnet": "databricks-gpt-5-5", "haiku": "databricks-claude-haiku-5-5",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("Models[%s] = %q, want %q", k, m[k], v)
		}
	}
	plain := mustParse(t, `{"providers":{"openai":{"p":{"protocol":"anthropic","base_url":"https://p","auth":{"type":"none"},"models":{"opus":"gpt-5.5"}}}}}`)
	if got := plain.Providers["p"].Models["opus"]; got != "gpt-5.5" {
		t.Errorf("plain provider model = %q", got)
	}
}

func TestLoadCacheReturnsIndependentRegistry(t *testing.T) {
	// Given a settled vault with a provider model and fallback list.
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"fallback":["ultra"]}`)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(vault.Path(), old, old); err != nil {
		t.Fatal(err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	wantModel := reg.Providers["ultra"].Models["opus"]
	wantFallback := slices.Clone(reg.Fallback)
	// When a caller mutates the returned model map and appends to fallback.
	reg.Providers["ultra"].Models["opus"] = "x"
	reg.Fallback = append(reg.Fallback, "caller-only")
	next, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Then another Load returns the unchanged cached registry.
	if got := next.Providers["ultra"].Models["opus"]; got != wantModel {
		t.Errorf("Providers[ultra].Models[opus] = %q, want %q", got, wantModel)
	}
	if !slices.Equal(next.Fallback, wantFallback) {
		t.Errorf("Fallback = %v, want %v", next.Fallback, wantFallback)
	}
}

func TestRegistryCloneReferenceFields(t *testing.T) {
	// Given a registry with every reference field populated, including private declaration order.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reg := Registry{order: []string{"order"}}
	populateCloneReferences(reflect.ValueOf(&reg).Elem())
	// When the real Clone copies all nested provider and routing types.
	cloned := reg.clone()
	// Then values are preserved and no map, slice or pointer field aliases its original.
	if !reflect.DeepEqual(&reg, cloned) {
		t.Fatal("Clone changed registry values")
	}
	assertCloneReferences(t, reflect.ValueOf(&reg), reflect.ValueOf(cloned), "Registry")
}

func populateCloneReferences(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for _, fv := range v.Fields() {
			if fv.CanSet() {
				populateCloneReferences(fv)
			}
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		populateCloneReferences(v.Elem())
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key := reflect.New(v.Type().Key()).Elem()
		value := reflect.New(v.Type().Elem()).Elem()
		populateCloneReferences(key)
		populateCloneReferences(value)
		v.SetMapIndex(key, value)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 2))
		populateCloneReferences(v.Index(0))
	case reflect.String:
		v.SetString("fixture")
	}
}

func assertCloneReferences(t *testing.T, original, cloned reflect.Value, field string) {
	t.Helper()
	switch original.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if original.IsNil() || cloned.IsNil() {
			t.Fatalf("%s: reference field must be populated in original and clone", field)
		}
		if original.UnsafePointer() == cloned.UnsafePointer() {
			t.Errorf("%s: Clone aliases original %s", field, original.Kind())
		}
	}
	walkCloneReferences(t, original, cloned, field)
}

func walkCloneReferences(t *testing.T, original, cloned reflect.Value, field string) {
	t.Helper()
	switch original.Kind() {
	case reflect.Struct:
		for i := 0; i < original.NumField(); i++ {
			name := field + "." + original.Type().Field(i).Name
			assertCloneReferences(t, original.Field(i), cloned.Field(i), name)
		}
	case reflect.Pointer:
		assertCloneReferences(t, original.Elem(), cloned.Elem(), field)
	case reflect.Map:
		for _, key := range original.MapKeys() {
			name := fmt.Sprintf("%s[%v]", field, key)
			assertCloneReferences(t, original.MapIndex(key), cloned.MapIndex(key), name)
		}
	case reflect.Slice:
		for i := 0; i < original.Len(); i++ {
			name := fmt.Sprintf("%s[%d]", field, i)
			assertCloneReferences(t, original.Index(i), cloned.Index(i), name)
		}
	}
}
