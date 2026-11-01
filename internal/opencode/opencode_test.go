package opencode

import (
	"encoding/json"
	"github.com/edotau/claude-code/internal/agentconfig"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestMutateOpencodeConfigEnvPolicies(t *testing.T) {
	set := func(c map[string]any) { c["k"] = "v" }
	bad := []string{"A=1", opencodeConfigEnv + "={nope"}
	if _, err := mutateOpencodeConfigEnv(append([]string(nil), bad...), false, set); err == nil {
		t.Error("strict: an unreadable block must be an error")
	}
	got, err := mutateOpencodeConfigEnv(append([]string(nil), bad...), true, set)
	if err != nil || got[1] != opencodeConfigEnv+`={"k":"v"}` || len(got) != 2 {
		t.Errorf("lenient = %v, %v; want the block replaced in place", got, err)
	}
	got, _ = mutateOpencodeConfigEnv([]string{"A=1", opencodeConfigEnv + `={"m":1}`}, false, set)
	if got[1] != opencodeConfigEnv+`={"k":"v","m":1}` {
		t.Errorf("merge = %v", got)
	}
	if got, _ = mutateOpencodeConfigEnv(nil, false, set); len(got) != 1 || got[0] != opencodeConfigEnv+`={"k":"v"}` {
		t.Errorf("absent = %v, want one appended entry", got)
	}
}

// TestOpencodeCredentialFallsThroughWhenOnlyAnotherLoginExists checks that when opencode has a login for
// another provider (openai) but none for its own "opencode" plan, an opencode-go cell (prefix "opencode-go"
// maps to credential id "opencode") falls through to the OPENCODE_API_KEY vault key, erroring when that is
// absent too — precedence never borrows the unrelated openai login.
func TestOpencodeCredentialFallsThroughWhenOnlyAnotherLoginExists(t *testing.T) {
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	t.Setenv(OpencodeAPIKeyEnv, "")
	writeAuthJSON(t, data, `{"openai":{"type":"oauth"}}`)
	if l := OpencodeLogins(); len(l) != 1 || l[0] != "openai" {
		t.Fatalf("precondition: logins = %v, want only openai", l)
	}
	env, err := opencodeCredential("opencode-go")
	if err == nil {
		t.Fatalf("env = %v, want an error: the openai login must not cover an opencode-go cell", env)
	}
	if !strings.Contains(err.Error(), OpencodeAPIKeyEnv) {
		t.Errorf("err = %v, want it to name %s", err, OpencodeAPIKeyEnv)
	}
}

// TestOpencodeCredentialVaultKeyOutranksMissingOwnLogin checks the same opencode-go cell resolves once the
// OPENCODE_API_KEY vault key is present, still with no "opencode" login stored.
func TestOpencodeCredentialVaultKeyOutranksMissingOwnLogin(t *testing.T) {
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	writeAuthJSON(t, data, `{"openai":{"type":"oauth"}}`)
	t.Setenv(OpencodeAPIKeyEnv, "sk-test-key")
	env, err := opencodeCredential("opencode-go")
	if err != nil {
		t.Fatalf("err = %v, want the vault/env key to resolve it", err)
	}
	if len(env) != 1 || env[0] != OpencodeAPIKeyEnv+"=sk-test-key" {
		t.Errorf("env = %v, want [%s=sk-test-key]", env, OpencodeAPIKeyEnv)
	}
}

// TestOpencodeOverlayModelComesFromTable checks the overlay's model/small_model are the routing table's opencode models.
func TestOpencodeOverlayModelComesFromTable(t *testing.T) {
	isolateOpencodeRouteEnv(t)
	vaulttest.Use(t)
	wantMain, wantSmall, ok := router.OpencodeTableModels()
	if !ok {
		t.Fatal("routing table has no opencode sonnet/haiku models")
	}
	_, cfgBody, _, _, err := agentconfig.OpencodeOverlayRendered()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(cfgBody, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != wantMain || doc["small_model"] != wantSmall {
		t.Errorf("model=%v small_model=%v, want the table's %s and %s", doc["model"], doc["small_model"], wantMain, wantSmall)
	}
}

// A copilot-only opencode launch renders github-copilot/<id> models, never an opencode-go id.
func TestOpencodeCopilotOnlyOverlayModels(t *testing.T) {
	data, _ := isolateOpencodeRouteEnv(t)
	vaulttest.Use(t)
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{"github-copilot":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"personal":{"protocol":"openai",`+
		`"base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},`+
		`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"mai-code-1.1-flash"}}}}}`)
	if OpencodeOwnLoginPresent() || !OpencodeCopilotLoginPresent() {
		t.Fatal("fixture must be copilot-only")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	main, small, ok := OpencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("no copilot model ids")
	}
	_, body, _, _, err := agentconfig.OpencodeOverlayRendered()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "github-copilot/"+main || doc["small_model"] != "github-copilot/"+small {
		t.Errorf("model=%v small_model=%v, want github-copilot/%s and github-copilot/%s", doc["model"], doc["small_model"], main, small)
	}
	for _, k := range []string{"model", "small_model"} {
		if s, _ := doc[k].(string); strings.HasPrefix(s, "opencode-go/") {
			t.Errorf("%s = %q, an opencode-go id on a copilot-only login", k, s)
		}
	}
}

// TestOpencodeOverlayModelsFollowTable checks the opencode overlay's model/small_model are the table's opencode cells,
// with a vault route override reaching only the overridden cell.
func TestOpencodeOverlayModelsFollowTable(t *testing.T) {
	vaulttest.Use(t)
	render := func() map[string]any {
		_, body, _, _, err := agentconfig.OpencodeOverlayRendered()
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	tb, err := router.Load()
	if err != nil {
		t.Fatal(err)
	}
	sonnet, _ := tb.Cell("opencode", "sonnet")
	haiku, _ := tb.Cell("opencode", "haiku")
	if doc := render(); doc["model"] != sonnet.Model || doc["small_model"] != haiku.Model {
		t.Errorf("overlay = %v/%v, want the table's %q/%q", doc["model"], doc["small_model"], sonnet.Model, haiku.Model)
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"routing":{"cells":{"opencode":{"sonnet":{"model":"github-copilot/overridden-model"}}}}}`)
	if doc := render(); doc["model"] != "github-copilot/overridden-model" || doc["small_model"] != haiku.Model {
		t.Errorf("overridden overlay = %v/%v, want github-copilot/overridden-model and unchanged %q", doc["model"], doc["small_model"], haiku.Model)
	}
}
