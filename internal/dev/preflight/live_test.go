package preflight

import (
	"io"

	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/driver/copilotauth"
	"github.com/edotau/claude-code/internal/routing/serving"

	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/catalog"
	"github.com/edotau/claude-code/internal/routing/providers"
)

func preflightRegistry(cred bool) *providers.Registry {
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks}
	if cred {
		p.Auth = providers.Auth{Type: "key", Command: "echo x"}
	}
	ag := &providers.Provider{Name: "antigravity", Provider: "google", Auth: providers.Auth{Type: providers.AuthCLI, Login: "gemini"}}
	return &providers.Registry{Providers: map[string]*providers.Provider{"dev": p, "antigravity": ag}}
}

// setPins sets agent record fields from "<AGENT>_<FIELD>" keys (CODEX_MODEL sets codex's model).
func setPins(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		agent, field, _ := strings.Cut(strings.ToLower(k), "_")
		vaulttest.SetRecord(t, agent, field, v)
	}
}

func byAgent(ps []Pin) map[string]Pin {
	out := map[string]Pin{}
	for _, p := range ps {
		out[p.Agent] = p
	}
	return out
}

// Bare pins match prefixed Databricks listings; prefixed pins never match bare listings.
func TestHasMatchesRuntimeSpelling(t *testing.T) {
	dev := preflightRegistry(true).Providers["dev"]
	if listed, _ := (serving.Served{Models: []serving.ServedModel{{ID: "databricks-my-endpoint", Ready: "READY"}}}).Has(dev, "my-endpoint"); !listed {
		t.Error("bare pin my-endpoint must match listed databricks-my-endpoint")
	}
	if listed, _ := (serving.Served{Models: []serving.ServedModel{{ID: "my-endpoint", Ready: "READY"}}}).Has(dev, "databricks-my-endpoint"); listed {
		t.Error("pin databricks-my-endpoint must not match a bare my-endpoint listing")
	}
}

// A listing stamped in the future is stale, not fresh forever.
func TestFutureListingIsStale(t *testing.T) {
	now := time.Now()
	if (serving.Served{FetchedAt: now.Add(48 * time.Hour)}).Fresh(now) {
		t.Fatal("future fetched_at must not be fresh")
	}
	if !(serving.Served{FetchedAt: now.Add(time.Minute)}).Fresh(now) {
		t.Fatal("small skew must stay fresh")
	}
}

// With no credential anywhere the install step is silent and never asks a model.
func TestInstallStepSkipsWithoutCredential(t *testing.T) {
	hermeticVault(t)
	pinAll(t)
	runAsk = func(context.Context, string, string, string) (wire.Envelope, error) {
		t.Error("ask must not run without a credential")
		return wire.Envelope{}, nil
	}
	reg := preflightRegistry(false)
	delete(reg.Providers, "antigravity") // a CLI-login pin counts as a credential
	var buf bytes.Buffer
	RunInstallStep(context.Background(), &buf, reg, "claude-code", time.Now(), false)
	if buf.Len() != 0 {
		t.Fatalf("want a silent skip, got %q", buf.String())
	}
}

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
// nopT lets TestMain install the default stub; it has no test to clean up after.
type nopT struct{}

func (nopT) Helper()        {}
func (nopT) Cleanup(func()) {}

func TestMain(m *testing.M) {
	copilotauth.StubRead(nopT{}, nil, nil) // no Copilot snapshot, as when copilot is not linked
	os.Exit(hosttest.Main(m))
}

func BenchmarkPrintFixRows(b *testing.B) {
	ps := []Pin{{Status: StatusOK}, {Status: statusStale}, {Status: StatusOK}, {Status: statusUnserved}}
	for b.Loop() {
		printFixRows(io.Discard, ps)
	}
}

func TestStaleGithubRows(t *testing.T) {
	now := time.Now()
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"gh": {Name: "gh", Provider: "github", Login: "me"}, "nologin": {Name: "nologin", Provider: "github"}}}
	for _, c := range []struct {
		at   time.Time
		want int
	}{{time.Time{}, 1}, {now.Add(-48 * time.Hour), 1}, {now.Add(-time.Minute), 0}} {
		copilotauth.StubRead(t, nil, func(string) time.Time { return c.at })
		if got := len(staleGithubRows(reg, now, false)); got != c.want {
			t.Errorf("snapshot at %v: %d stale rows, want %d", c.at, got, c.want)
		}
	}
}

// An unpinned Databricks profile with a past-TTL listing is re-listed by a non-force refresh.
func TestRefreshStaleRelistsUnpinnedStaleProfile(t *testing.T) {
	hermeticVault(t)
	reg, now := preflightRegistry(true), time.Now()
	var got []string
	refresh = func(_ context.Context, p *providers.Provider) error { got = append(got, p.Name); return nil }
	t.Cleanup(func() { refresh = catalog.Refresh })
	stale := serving.Served{Schema: 1, Profile: "dev", FetchedAt: now.Add(-26 * time.Hour), Models: []serving.ServedModel{{ID: "m"}}}
	if err := serving.Save(stale); err != nil {
		t.Fatal(err)
	}
	if errs := RefreshStale(context.Background(), reg, nil, now, false); len(errs) != 0 || len(got) != 1 || got[0] != "dev" {
		t.Fatalf("errs=%v relisted=%v, want [dev]", errs, got)
	}
}
