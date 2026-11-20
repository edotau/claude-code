package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func pickerScratch(t *testing.T) {
	t.Helper()
	hosttest.PinScratch(t)
	vaulttest.UseKeyring(t)
}

// TestDiscoverPickerProfilesSkipsUnusableRows: a keyless API row and an empty listing are skips, never errors.
func TestDiscoverPickerProfilesSkipsUnusableRows(t *testing.T) {
	pickerScratch(t)
	listings := map[string][]providers.ModelInfo{"listed": {{ID: "live-model-a"}, {ID: "live-model-b"}}, "empty": nil}
	github := func(_ context.Context, p *providers.Provider) ([]providers.ModelInfo, error) {
		live, ok := listings[p.Name]
		if !ok {
			return nil, errors.New("catalog request failed (HTTP 503) for " + p.Name)
		}
		return live, nil
	}
	gh := func(name string) *providers.Provider {
		return &providers.Provider{Name: name, Provider: "github", Login: "someone", Auth: providers.Auth{Type: providers.AuthCLI}}
	}
	keyless := &providers.Provider{Name: "keyless", Provider: "anthropic", Protocol: providers.ProtocolAnthropic,
		BaseURL: "https://api.anthropic.com", Auth: providers.Auth{Type: providers.AuthKey, Env: "ANTHROPIC_API_KEY"}}
	for _, tc := range []struct {
		p              *providers.Provider
		status, note   string
		wantDiscovered int
	}{
		// never reaches the stub: "keyless" would be a 503 error
		{keyless, "skipped", "vault set ANTHROPIC_KEYLESS_KEY", 0},
		{gh("empty"), "skipped", "no models", 0},
		{gh("listed"), "preview", "", 2},
		{gh("down"), "error", "HTTP 503", 0},
	} {
		r := discoverPickerProfile(tc.p, github, false, false)
		if r.Status != tc.status || !strings.Contains(r.Note, tc.note) || r.Discovered != tc.wantDiscovered {
			t.Errorf("%s = %q discovered=%d note=%q, want %q discovered=%d note containing %q",
				tc.p.Name, r.Status, r.Discovered, r.Note, tc.status, tc.wantDiscovered, tc.note)
		}
	}
}

// Given a deprecated-wording 400, Then apply drops that id, a later apply does not re-add it, other failures change nothing
// and a preview sends no probe.
func TestDiscoverApplyDropsDeprecatedEndpoints(t *testing.T) {
	pickerScratch(t)
	var live []providers.ModelInfo
	for _, id := range []string{"databricks-old-model", "databricks-fresh-model", "databricks-flaky-model", "databricks-cut-model"} {
		live = append(live, providers.ModelInfo{ID: id, Task: "llm/v1/chat", Ready: "READY",
			APITypes: []string{"mlflow/v1/chat/completions", "openai/v1/responses"}})
	}
	orig := discoverGateway
	t.Cleanup(func() { discoverGateway = orig })
	discoverGateway = func(context.Context, *providers.Provider) ([]providers.ModelInfo, error) { return live, nil }
	sent := stubProbe(t)
	p := &providers.Provider{Name: "devhost", Provider: "databricks", Preset: providers.PresetDatabricks,
		Host: "https://devhost.example.com", Auth: providers.Auth{Type: providers.AuthOAuth}}
	if r := discoverPickerProfile(p, nil, false, false); len(*sent) != 0 || !strings.Contains(r.ProbeNote, "no deprecation probe") {
		t.Fatalf("preview sent %d probes, note %q; want none and a note", len(*sent), r.ProbeNote)
	}
	r := discoverPickerProfile(p, nil, true, false)
	if got := fmt.Sprint(r.Deprecated, len(*sent), (*sent)[0].Responses); got != "[databricks-old-model] 4 true" {
		t.Fatalf("apply: deprecated, probes, responses route = %s", got)
	}
	saved, _ := providers.PickerModels(p)
	var ids []string
	for _, m := range saved {
		ids = append(ids, m.ID)
	}
	if want := []string{"databricks-cut-model", "databricks-flaky-model", "databricks-fresh-model"}; !slices.Equal(ids, want) {
		t.Fatalf("saved = %v, want %v", ids, want)
	}
	if want := "claude-code models discover --drop databricks-cut-model --apply"; !strings.Contains(r.ProbeNote, want) {
		t.Errorf("probe note = %q, want it to name %q", r.ProbeNote, want)
	}
	if again := discoverPickerProfile(p, nil, true, false); len(again.Deprecated) != 0 || len(again.Additions) != 0 {
		t.Errorf("second apply = %+v, want the dropped id not re-added", again)
	}
}

// stubProbe answers the deprecation wording for old-model, another 400 for flaky-model and 200 otherwise.
func stubProbe(t *testing.T) *[]serving.CheckTarget {
	t.Helper()
	sent := new([]serving.CheckTarget)
	orig := probeEndpoints
	t.Cleanup(func() { probeEndpoints = orig })
	replies := map[string]serving.CheckResult{
		"databricks-old-model": {Status: serving.StatusFail, HTTP: 400,
			Error: `HTTP 400: {"error_code":"BAD_REQUEST","message":"This endpoint databricks-old-model is deprecated."}`},
		"databricks-flaky-model": {Status: serving.StatusFail, HTTP: 400, Error: `HTTP 400: {"message":"bad request shape"}`},
		"databricks-cut-model":   {Status: serving.StatusBroken, HTTP: 200, Error: "stream ended without [DONE] after 1 events, 40 bytes"},
	}
	probeEndpoints = func(_ context.Context, _ *providers.Provider, ts []serving.CheckTarget, _ serving.CheckOptions) ([]serving.CheckResult, error) {
		*sent = append(*sent, ts...)
		out := make([]serving.CheckResult, len(ts))
		for i, tg := range ts {
			out[i] = replies[tg.ID]
			if out[i].Status == "" {
				out[i] = serving.CheckResult{Status: serving.StatusOK, HTTP: 200}
			}
			out[i].ID = tg.ID
		}
		return out, nil
	}
	return sent
}
