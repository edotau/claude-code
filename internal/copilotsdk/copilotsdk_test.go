package copilotsdk

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
)

// funcName compares function VALUES by the symbol they point to (PermissionHandlerFunc values are not
// comparable with ==); it tells approveToolOnce apart from denyAllTools.
func funcName(f any) string {
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

// EnableMemory turns into SessionConfig.Memory only when set, and permissionHandler follows AllowAllTools
// alone — a session (new or resumed) must carry both consistently.
func TestBuildSessionConfigMemoryAndPermission(t *testing.T) {
	cfg := BuildSessionConfig(Options{EnableMemory: true, AllowAllTools: true})
	if cfg.Memory == nil || !cfg.Memory.Enabled {
		t.Errorf("Memory = %+v, want enabled", cfg.Memory)
	}
	if funcName(cfg.OnPermissionRequest) != funcName(approveToolOnce) {
		t.Errorf("OnPermissionRequest = %s, want approveToolOnce (AllowAllTools=true)", funcName(cfg.OnPermissionRequest))
	}

	off := BuildSessionConfig(Options{})
	if off.Memory != nil {
		t.Errorf("Memory = %+v, want nil (EnableMemory=false)", off.Memory)
	}
	if funcName(off.OnPermissionRequest) != funcName(denyAllTools) {
		t.Errorf("OnPermissionRequest = %s, want denyAllTools (AllowAllTools=false)", funcName(off.OnPermissionRequest))
	}

	resumed := buildResumeSessionConfig(Options{EnableMemory: true})
	if resumed.Memory == nil || !resumed.Memory.Enabled {
		t.Errorf("resumed Memory = %+v, want enabled (mirrors BuildSessionConfig)", resumed.Memory)
	}
}

// vendors.copilot blanks each BYOK credential var, then appends the real one: the session must get the last value.
func TestProviderFromEnvLastDuplicateWins(t *testing.T) {
	p := providerFromEnv([]string{
		"COPILOT_PROVIDER_BASE_URL=https://h/serving-endpoints", "COPILOT_PROVIDER_TYPE=openai",
		"COPILOT_PROVIDER_API_KEY=", "COPILOT_PROVIDER_BEARER_TOKEN=", "COPILOT_PROVIDER_BEARER_TOKEN=tok",
	})
	if p == nil || p.BearerToken != "tok" || p.APIKey != "" {
		t.Fatalf("provider = %+v, want BearerToken tok and no APIKey", p)
	}
}

// A gateway Claude id keeps its wire name but gets copilot's catalog ModelID; other ids pass through.
func TestProviderFromEnvWellKnownModelID(t *testing.T) {
	for id, want := range map[string]string{
		"databricks-claude-sonnet-5-5": "claude-sonnet-5.5",
		"databricks-claude-opus-5-5":   "claude-opus-5.5",
		"claude-opus-5.5":              "claude-opus-5.5",
		"gpt-5":                        "gpt-5",
	} {
		p := providerFromEnv([]string{"COPILOT_PROVIDER_BASE_URL=https://h", "COPILOT_MODEL=" + id})
		if p.ModelID != want || p.WireModel != id {
			t.Errorf("%s: ModelID=%q WireModel=%q, want %q / %q", id, p.ModelID, p.WireModel, want, id)
		}
	}
}

// An explicit MODEL_ID and WIRE_API from the launcher win over the well-known mapping.
func TestProviderFromEnvResponsesWire(t *testing.T) {
	p := providerFromEnv([]string{
		"COPILOT_PROVIDER_BASE_URL=http://r/p/dev/responses", "COPILOT_PROVIDER_WIRE_API=responses",
		"COPILOT_PROVIDER_MODEL_ID=x", "COPILOT_MODEL=databricks-gpt-6-1-sol",
	})
	if p == nil || p.WireAPI != "responses" || p.ModelID != "x" {
		t.Fatalf("provider = %+v, want WireAPI responses and ModelID x", p)
	}
}

// ListModels maps the SDK's raw catalog into the harness's own Model shape, field for field.
func TestListModelsMapsFields(t *testing.T) {
	mult := 2.5
	old := listModelsFn
	defer func() { listModelsFn = old }()
	listModelsFn = func(ctx context.Context, o Options) ([]copilot.ModelInfo, error) {
		return []copilot.ModelInfo{
			{
				ID:   "claude-sonnet-5",
				Name: "Claude Sonnet 5",
				Capabilities: copilot.ModelCapabilities{
					Supports: copilot.ModelSupports{Vision: true, ReasoningEffort: false},
				},
				Billing: &copilot.ModelBilling{Multiplier: &mult},
			},
			{
				ID:   "gpt-5-mini",
				Name: "GPT-5 mini",
				Capabilities: copilot.ModelCapabilities{
					Supports: copilot.ModelSupports{Vision: false, ReasoningEffort: true},
				},
			},
		}, nil
	}
	got, err := ListModels(context.Background(), Options{CLIPath: "/fake/copilot"})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2", len(got))
	}
	if got[0].ID != "claude-sonnet-5" || got[0].Name != "Claude Sonnet 5" || !got[0].Vision || got[0].ReasoningEffort {
		t.Errorf("model[0] = %+v", got[0])
	}
	if got[0].Multiplier == nil || *got[0].Multiplier != mult {
		t.Errorf("model[0].Multiplier = %v, want %v", got[0].Multiplier, mult)
	}
	if got[1].ID != "gpt-5-mini" || got[1].Vision || !got[1].ReasoningEffort {
		t.Errorf("model[1] = %+v", got[1])
	}
	if got[1].Multiplier != nil {
		t.Errorf("model[1].Multiplier = %v, want nil (no billing block)", *got[1].Multiplier)
	}
}

// ListModels also maps SupportedReasoningEfforts/DefaultReasoningEffort, MaxContextWindowTokens, and the
// Policy.State enabled gate — Policy nil and Policy.State=="enabled" both count as enabled.
func TestListModelsMapsEffortsContextAndEnabled(t *testing.T) {
	old := listModelsFn
	defer func() { listModelsFn = old }()
	ctxTokens := 1_000_000
	listModelsFn = func(ctx context.Context, o Options) ([]copilot.ModelInfo, error) {
		return []copilot.ModelInfo{
			{ // no Policy at all: enabled by default
				ID: "claude-sonnet-5",
				Capabilities: copilot.ModelCapabilities{
					Limits: copilot.ModelLimits{MaxContextWindowTokens: &ctxTokens},
				},
				SupportedReasoningEfforts: []string{"low", "medium", "high"},
				DefaultReasoningEffort:    "medium",
			},
			{ // explicit Policy.State == "enabled"
				ID:     "gpt-5-mini",
				Policy: &copilot.ModelPolicy{State: "enabled"},
			},
			{ // disabled by policy
				ID:     "retired-model",
				Policy: &copilot.ModelPolicy{State: "disabled"},
			},
			{ // no context limit reported at all
				ID: "auto",
			},
		}, nil
	}
	got, err := ListModels(context.Background(), Options{CLIPath: "/fake/copilot"})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d models, want 4", len(got))
	}
	sonnet := got[0]
	if want := []string{"low", "medium", "high"}; !reflect.DeepEqual(sonnet.Efforts, want) {
		t.Errorf("sonnet Efforts = %v, want %v", sonnet.Efforts, want)
	}
	if sonnet.DefaultEffort != "medium" {
		t.Errorf("sonnet DefaultEffort = %q, want medium", sonnet.DefaultEffort)
	}
	if sonnet.ContextTokens != ctxTokens {
		t.Errorf("sonnet ContextTokens = %d, want %d", sonnet.ContextTokens, ctxTokens)
	}
	if !sonnet.Enabled {
		t.Error("sonnet (no Policy) Enabled = false, want true (nil policy means enabled)")
	}
	if !got[1].Enabled {
		t.Error(`gpt-5-mini (Policy.State=="enabled") Enabled = false, want true`)
	}
	if got[2].Enabled {
		t.Error(`retired-model (Policy.State=="disabled") Enabled = true, want false`)
	}
	if got[3].ContextTokens != 0 {
		t.Errorf("auto ContextTokens = %d, want 0 (no limit reported)", got[3].ContextTokens)
	}
}

// CatalogOptions prefers an explicit token (GitHubToken) over UseLoggedInUser whenever the token is
// non-empty; an empty token falls back to the CLI's own stored login.
func TestCatalogOptionsPrefersToken(t *testing.T) {
	withToken := CatalogOptions("/fake/copilot", "gh-token-123")
	if withToken.GitHubToken != "gh-token-123" {
		t.Errorf("GitHubToken = %q, want gh-token-123", withToken.GitHubToken)
	}
	if withToken.UseLoggedInUser {
		t.Error("UseLoggedInUser = true with a token present, want false (token wins)")
	}
	noToken := CatalogOptions("/fake/copilot", "")
	if noToken.GitHubToken != "" {
		t.Errorf("GitHubToken = %q, want empty", noToken.GitHubToken)
	}
	if !noToken.UseLoggedInUser {
		t.Error("UseLoggedInUser = false with no token, want true (fall back to stored login)")
	}
}

// A lister failure (no login, CLI missing, RPC error) surfaces as a wrapped error naming the fix.
func TestListModelsWrapsListerError(t *testing.T) {
	old := listModelsFn
	defer func() { listModelsFn = old }()
	wantErr := errors.New("rpc: not authenticated")
	listModelsFn = func(ctx context.Context, o Options) ([]copilot.ModelInfo, error) {
		return nil, wantErr
	}
	_, err := ListModels(context.Background(), Options{CLIPath: "/fake/copilot"})
	if err == nil {
		t.Fatal("ListModels: want error, got nil")
	}
	if !strings.Contains(err.Error(), "sign in with `copilot`") {
		t.Errorf("error %q does not name the fix", err.Error())
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error %v does not wrap the lister's error %v", err, wantErr)
	}
}

// An empty catalog is not an error: ListModels returns an empty, non-nil slice.
func TestListModelsEmptyCatalog(t *testing.T) {
	old := listModelsFn
	defer func() { listModelsFn = old }()
	listModelsFn = func(ctx context.Context, o Options) ([]copilot.ModelInfo, error) {
		return []copilot.ModelInfo{}, nil
	}
	got, err := ListModels(context.Background(), Options{CLIPath: "/fake/copilot"})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d models, want 0", len(got))
	}
}
