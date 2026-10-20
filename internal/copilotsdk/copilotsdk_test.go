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
