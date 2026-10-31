package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type discoveryTransport func(*http.Request) (*http.Response, error)

func (f discoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type discoveryFake struct {
	me, list atomic.Int32
	identity atomic.Value
	status   atomic.Int32
	listCode atomic.Int32
	auth     atomic.Value
}

func discoveryEnv(t *testing.T) (*providers.Provider, *discoveryFake) {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DISCOVERY_TEST_TOKEN", "synthetic-discovery-token")
	f := &discoveryFake{}
	f.identity.Store(`{"id":"subject-one","active":true}`)
	f.status.Store(200)
	f.listCode.Store(200)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.auth.Store(r.Header.Get("Authorization"))
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/2.0/preview/scim/v2/Me":
			f.me.Add(1)
			if r.URL.Query().Get("attributes") != "id,active" || len(r.URL.Query()) != 1 {
				t.Errorf("identity query = %q", r.URL.RawQuery)
			}
			w.WriteHeader(int(f.status.Load()))
			_, _ = w.Write([]byte(f.identity.Load().(string)))
		case "/api/2.0/serving-endpoints":
			f.list.Add(1)
			w.WriteHeader(int(f.listCode.Load()))
			_, _ = w.Write([]byte(`{"endpoints":[{"name":"databricks-encrypted-sentinel","task":"llm/v1/chat","state":{"ready":"READY"}}]}`))
		default:
			t.Errorf("unexpected HTTP path %s", r.URL.Path)
			http.Error(w, "unexpected path", 404)
		}
	}))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = discoveryTransport(func(r *http.Request) (*http.Response, error) {
		if "https://"+r.URL.Host != srv.URL {
			return nil, errors.New("test blocked nonlocal request")
		}
		return srv.Client().Transport.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	return &providers.Provider{Name: "discovery-test", Preset: providers.PresetDatabricks, Host: srv.URL,
		Auth: providers.Auth{Type: providers.AuthKey, Env: "DISCOVERY_TEST_TOKEN"}}, f
}

func discoverForTest(t *testing.T, p *providers.Provider, refresh bool) ServingDiscovery {
	t.Helper()
	s, err := DiscoverServingModels(context.Background(), p, refresh)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestServingDiscoveryAuthenticatedHitAndRefresh(t *testing.T) {
	p, f := discoveryEnv(t)
	s := discoverForTest(t, p, false)
	if s.CacheHit || s.Schema != 1 || s.DiscoveredAt.IsZero() || len(s.Models) != 1 || f.me.Load() != 1 || f.list.Load() != 1 {
		t.Fatalf("first discovery: %+v, Me=%d listing=%d", s, f.me.Load(), f.list.Load())
	}
	if _, found, err := vault.ReadSecret(servingDiscoveryKey(s.Scope)); err != nil || found {
		t.Fatalf("discovery alone persisted cache: found=%v err=%v", found, err)
	}
	if err := SaveServingDiscovery(p, s); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISCOVERY_TEST_TOKEN", "rotated-synthetic-token")
	hit := discoverForTest(t, p, false)
	if !hit.CacheHit || !reflect.DeepEqual(hit.Models, s.Models) || !hit.DiscoveredAt.Equal(s.DiscoveredAt) ||
		f.me.Load() != 2 || f.list.Load() != 1 || f.auth.Load() != "Bearer rotated-synthetic-token" {
		t.Fatalf("rotated-token hit: %+v, Me=%d listing=%d", hit, f.me.Load(), f.list.Load())
	}
	if fresh := discoverForTest(t, p, true); fresh.CacheHit || f.me.Load() != 3 || f.list.Load() != 2 {
		t.Fatalf("refresh: hit=%v Me=%d listing=%d", fresh.CacheHit, f.me.Load(), f.list.Load())
	}
	before, _, err := vault.ReadSecret(servingDiscoveryKey(s.Scope))
	if err != nil {
		t.Fatal(err)
	}
	f.listCode.Store(503)
	if _, err := DiscoverServingModels(context.Background(), p, true); err == nil || strings.Contains(err.Error(), "encrypted-sentinel") {
		t.Fatalf("failed refresh must fail without leaking body: %v", err)
	}
	after, _, err := vault.ReadSecret(servingDiscoveryKey(s.Scope))
	if err != nil || before != after || f.me.Load() != 4 || f.list.Load() != 3 {
		t.Fatalf("failed refresh changed cache or skipped HTTP: err=%v Me=%d listing=%d", err, f.me.Load(), f.list.Load())
	}
}

func TestServingDiscoveryRejectsInvalidIdentityDespiteCache(t *testing.T) {
	p, f := discoveryEnv(t)
	s := discoverForTest(t, p, false)
	if err := SaveServingDiscovery(p, s); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		code int32
		body string
	}{
		{"revoked", 401, `synthetic-discovery-token upstream-private-body`},
		{"inactive", 200, `{"id":"subject-one","active":false}`},
		{"empty-id", 200, `{"id":"  ","active":true}`},
		{"malformed", 200, `upstream-private-body`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.status.Store(tc.code)
			f.identity.Store(tc.body)
			before := f.me.Load()
			_, err := DiscoverServingModels(context.Background(), p, false)
			if err == nil || f.me.Load() != before+1 || f.list.Load() != 1 {
				t.Fatalf("invalid identity accepted or listed: err=%v Me=%d listing=%d", err, f.me.Load(), f.list.Load())
			}
			for _, forbidden := range []string{"synthetic-discovery-token", "upstream-private-body"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Error("identity error leaked private data")
				}
			}
		})
	}
	f.status.Store(200)
	f.identity.Store(`{"id":"subject-one"}`)
	if hit := discoverForTest(t, p, false); !hit.CacheHit || f.list.Load() != 1 {
		t.Fatal("omitted active should accept the authenticated subject's cache")
	}
}

func TestServingDiscoveryScopeIsolation(t *testing.T) {
	p, f := discoveryEnv(t)
	s := discoverForTest(t, p, false)
	if err := SaveServingDiscovery(p, s); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*providers.Provider)
	}{
		{"profile", func(p *providers.Provider) { p.Name = "other-profile" }},
		{"cli-profile", func(p *providers.Provider) { p.CLIProfile = "other-cli-profile" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := *p
			tc.edit(&other)
			before := f.list.Load()
			if got := discoverForTest(t, &other, false); got.CacheHit || f.list.Load() != before+1 {
				t.Fatal("changed scope reused another cache")
			}
			if err := SaveServingDiscovery(&other, s); err == nil {
				t.Fatal("accepted snapshot belonging to another scope")
			}
		})
	}
	f.identity.Store(`{"id":"subject-two","active":true}`)
	if got := discoverForTest(t, p, false); got.CacheHit || got.Scope.SubjectID != "subject-two" || f.list.Load() != 4 {
		t.Fatal("different authenticated subject reused discovery")
	}
	f.identity.Store(`{"id":"subject-one","active":true}`)
	p.Host += "/"
	if got := discoverForTest(t, p, false); !got.CacheHit || f.list.Load() != 4 {
		t.Fatal("equivalent normalized host missed the cache")
	}
	// Scope construction must distinguish hosts without making a request to another workspace.
	other := *p
	other.Host = "https://other-workspace.invalid"
	if err := SaveServingDiscovery(&other, s); err == nil {
		t.Fatal("accepted snapshot belonging to another host")
	}
	other = *p
	other.Options.Wire = providers.WireAIGateway
	before := f.me.Load()
	if _, err := DiscoverServingModels(context.Background(), &other, false); err == nil || f.me.Load() != before {
		t.Fatal("gateway must be rejected before HTTP/cache access")
	}
	other = *p
	other.Preset = ""
	if _, err := DiscoverServingModels(context.Background(), &other, false); err == nil || f.me.Load() != before {
		t.Fatal("non-Databricks profile must be rejected before HTTP/cache access")
	}
}

func TestServingDiscoverySealsAndSanitizesSnapshot(t *testing.T) {
	p, _ := discoveryEnv(t)
	s := discoverForTest(t, p, false)
	s.CheckedAt = time.Now().UTC()
	s.Checks = []CheckResult{
		{ID: "encrypted-sentinel", Status: "fail", HTTP: 403, LatencyMS: 7, Tools: "private-tool-text", Error: "raw-upstream-body synthetic-discovery-token"},
		{ID: "accepted", Status: "ok", HTTP: 200, Tools: "yes"},
		{ID: "drop", Status: "raw-status-body"},
	}
	if err := SaveServingDiscovery(p, s); err != nil {
		t.Fatal(err)
	}
	name := servingDiscoveryKey(s.Scope)
	if err := vault.View(func(tx *vault.Tx) error {
		r, found, err := tx.Record(name)
		if err != nil {
			return err
		}
		if !found || len(r.Sealed) == 0 || r.Kid == "" || r.Prefix != "" {
			t.Error("snapshot is not a sealed record with empty prefix")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, found, err := vault.ReadSecret(name)
	if err != nil || !found {
		t.Fatalf("ReadSecret: found=%v err=%v", found, err)
	}
	var saved ServingDiscovery
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	want := []CheckResult{{ID: "encrypted-sentinel", Status: "fail", HTTP: 403, LatencyMS: 7}, {ID: "accepted", Status: "ok", HTTP: 200, Tools: "yes"}}
	if !reflect.DeepEqual(saved.Checks, want) || !reflect.DeepEqual(saved.Models, s.Models) || !saved.CheckedAt.Equal(s.CheckedAt) {
		t.Fatalf("saved snapshot lost models/measurements or retained unsafe fields: %+v", saved)
	}
	for _, forbidden := range []string{"raw-upstream-body", "synthetic-discovery-token", "private-tool-text", "raw-status-body"} {
		if strings.Contains(raw, forbidden) {
			t.Error("decrypted snapshot retains private diagnostics")
		}
	}
	disk, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"encrypted-sentinel", "subject-one", "synthetic-discovery-token", "raw-upstream-body"} {
		if strings.Contains(string(disk), forbidden) {
			t.Error("vault bytes contain plaintext snapshot data")
		}
	}
	if hit := discoverForTest(t, p, false); !hit.CacheHit || !reflect.DeepEqual(hit.Checks, want) {
		t.Fatal("encrypted historical checks did not survive authenticated cache read")
	}
}

func TestServingDiscoveryResolvedRoutesSeparateHistoricalChecks(t *testing.T) {
	p, f := discoveryEnv(t)
	p.Routes = map[string]string{
		providers.ProtocolAnthropic: p.Host + "/serving-endpoints/anthropic",
		providers.ProtocolOpenAI:    p.Host + "/serving-endpoints/chat",
		providers.ProtocolResponses: p.Host + "/serving-endpoints/responses",
	}
	s := discoverForTest(t, p, false)
	s.CheckedAt = time.Now().UTC()
	s.Checks = []CheckResult{{ID: "encrypted-sentinel", Status: "fail", HTTP: 400}}
	if err := SaveServingDiscovery(p, s); err != nil {
		t.Fatal(err)
	}
	for _, dialect := range []string{providers.ProtocolAnthropic, providers.ProtocolOpenAI, providers.ProtocolResponses} {
		t.Run(dialect, func(t *testing.T) {
			other := *p
			other.Routes = maps.Clone(p.Routes)
			other.Routes[dialect] += "/changed"
			before := f.list.Load()
			got := discoverForTest(t, &other, false)
			if got.CacheHit || len(got.Checks) != 0 || !got.CheckedAt.IsZero() || f.list.Load() != before+1 {
				t.Fatalf("changed %s reused historical route results: %+v", dialect, got)
			}
		})
	}
	if got := discoverForTest(t, p, false); !got.CacheHit || !reflect.DeepEqual(got.Checks, s.Checks) {
		t.Fatal("original route snapshot was not preserved")
	}
}

func TestServingDiscoveryRefreshCannotBypassUnreadableVault(t *testing.T) {
	for _, name := range []string{"corrupt-ciphertext", "locked-keyring"} {
		t.Run(name, func(t *testing.T) {
			p, f := discoveryEnv(t)
			ring := vaulttest.Keyring(t)
			s := discoverForTest(t, p, false)
			if err := SaveServingDiscovery(p, s); err != nil {
				t.Fatal(err)
			}
			key := servingDiscoveryKey(s.Scope)
			if name == "corrupt-ciphertext" {
				if err := vault.Update(func(tx *vault.Tx) error {
					r, found, err := tx.Record(key)
					if err != nil {
						return err
					}
					if !found || len(r.Sealed) == 0 {
						return errors.New("fixture lacks sealed discovery")
					}
					r.Sealed[len(r.Sealed)-1] ^= 0xff
					return tx.PutRecord(key, r)
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("HARNESS_VAULT_KEY", "")
				ring.Down = true
			}
			if _, _, err := vault.ReadSecret(key); err == nil {
				t.Fatal("negative control: cache should be unreadable")
			}
			before, err := os.ReadFile(vault.Path())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DiscoverServingModels(context.Background(), p, true); err == nil || f.me.Load() != 2 || f.list.Load() != 1 {
				t.Fatalf("refresh bypassed unreadable vault: err=%v Me=%d listing=%d", err, f.me.Load(), f.list.Load())
			}
			after, err := os.ReadFile(vault.Path())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("failed refresh changed unreadable vault: %v", err)
			}
		})
	}
}
