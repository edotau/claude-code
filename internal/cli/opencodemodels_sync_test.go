package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const syncListing = `{"endpoints":[
{"name":"databricks-sync-zeta","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-alpha","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-alpha","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-gpt-6-astra","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-gpt-5-5","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-not-ready","task":"llm/v1/chat","state":{"ready":"NOT_READY"}},
{"name":"databricks-sync-embed","task":"llm/v1/embeddings","state":{"ready":"READY"}},
{"name":"unprefixed-chat","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-marked[1m]","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-spaced ","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-","task":"llm/v1/chat","state":{"ready":"READY"}}]}`

type syncTransport func(*http.Request) (*http.Response, error)

func (f syncTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type syncFake struct {
	p        *providers.Provider
	me, list atomic.Int32
	meCode   atomic.Int32
	listCode atomic.Int32
	listing  atomic.Value
}

func syncEnv(t *testing.T, probe ...http.HandlerFunc) *syncFake {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DATABRICKS_SYNCTEST_PAT", "")
	f := &syncFake{}
	f.meCode.Store(200)
	f.listCode.Store(200)
	f.listing.Store(syncListing)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-sync-credential" ||
			(strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet) {
			t.Error("expected vault credential and GET for discovery/identity")
		}
		switch r.URL.Path {
		case "/api/2.0/preview/scim/v2/Me":
			f.me.Add(1)
			if r.URL.Query().Get("attributes") != "id,active" {
				t.Errorf("Me query = %s", r.URL.RawQuery)
			}
			w.WriteHeader(int(f.meCode.Load()))
			if f.meCode.Load() != 200 {
				_, _ = w.Write([]byte("private-upstream-body synthetic-sync-credential"))
				return
			}
			_, _ = w.Write([]byte(`{"id":"sync-subject","active":true}`))
		case "/api/2.0/serving-endpoints":
			f.list.Add(1)
			w.WriteHeader(int(f.listCode.Load()))
			_, _ = w.Write([]byte(f.listing.Load().(string)))
		default:
			if len(probe) > 0 {
				probe[0](w, r)
				return
			}
			t.Errorf("sync without --verify made unexpected request: %s", r.URL.Path)
			http.Error(w, "unexpected path", 404)
		}
	}))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = syncTransport(func(r *http.Request) (*http.Response, error) {
		if "https://"+r.URL.Host != srv.URL {
			return nil, errors.New("test blocked nonlocal request")
		}
		return srv.Client().Transport.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	if err := providers.SetProviderFields("synctest", map[string]any{
		"preset": "databricks", "host": srv.URL, "auth": map[string]any{"type": "pat"},
	}); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	f.p, err = reg.Get("synctest")
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.Secret(t, providers.AuthSecretName(f.p), "synthetic-sync-credential")
	return f
}

func syncVaultBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func syncRun(t *testing.T, args ...string) string {
	t.Helper()
	code, stdout, stderr := ocMenu(t, append([]string{"sync", "--profile", "synctest"}, args...)...)
	if code != 0 {
		t.Fatalf("sync %v: code=%d stderr=%s", args, code, stderr)
	}
	if strings.Contains(stdout+stderr, "synthetic-sync-credential") {
		t.Fatal("sync exposed credentials")
	}
	return stdout
}

func TestOpencodeMenuSyncPreviewApplyFiltersAndIdempotence(t *testing.T) {
	f := syncEnv(t)
	existing := []providers.OpencodeModel{
		{ID: "retained-absent", Label: "Keep first", Responses: true},
		{ID: "gpt-5-5", Label: "Explicit chat override", Responses: false},
		{ID: "glm-5-2", Label: "Explicit Responses override", Responses: true},
	}
	if err := providers.SetOpencodeMenu(existing); err != nil {
		t.Fatal(err)
	}
	before := syncVaultBytes(t)
	for _, args := range [][]string{nil, {"--dry-run"}} {
		out := syncRun(t, args...)
		if !strings.Contains(out, "+ sync-alpha") || !strings.Contains(out, "dry run") || !bytes.Equal(before, syncVaultBytes(t)) {
			t.Fatalf("preview wrote vault or omitted additions: %s", out)
		}
	}
	syncRun(t, "--apply")
	got := providers.OpencodeMenu()
	if len(got) != 6 || !reflect.DeepEqual(got[:3], existing) {
		t.Fatalf("additive merge changed existing entries or included rejected endpoints: %+v", got)
	}
	wantNew := []providers.OpencodeModel{
		{ID: "gpt-6-astra", Label: "GPT 6 Astra", Responses: true},
		{ID: "sync-alpha", Label: "Sync Alpha"},
		{ID: "sync-zeta", Label: "Sync Zeta"},
	}
	if !reflect.DeepEqual(got[3:], wantNew) {
		t.Fatalf("new models must be canonical, unique, sorted with inferred Responses: %+v", got[3:])
	}
	if f.me.Load() != 3 || f.list.Load() != 3 {
		t.Fatalf("previews must not cache: Me=%d listing=%d", f.me.Load(), f.list.Load())
	}
	sealed := syncVaultBytes(t)
	out := syncRun(t, "--apply")
	if f.me.Load() != 4 || f.list.Load() != 3 || !strings.Contains(out, "encrypted cache hit") || !bytes.Equal(sealed, syncVaultBytes(t)) {
		t.Fatalf("unchanged apply must authenticate, not list or write: Me=%d listing=%d out=%s", f.me.Load(), f.list.Load(), out)
	}
}

func TestOpencodeMenuSyncNoAdditionsStillSealsDiscovery(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", `{"endpoints":[]}`},
		{"already-in-menu", `{"endpoints":[{"name":"databricks-kimi-k3","task":"llm/v1/chat","state":{"ready":"READY"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := syncEnv(t)
			f.listing.Store(tc.body)
			before := syncVaultBytes(t)
			syncRun(t, "--dry-run")
			if !bytes.Equal(before, syncVaultBytes(t)) {
				t.Fatal("no-additions preview wrote cache")
			}
			out := syncRun(t, "--apply")
			if !strings.Contains(out, "no menu change") || providers.OpencodeMenuFromVault() {
				t.Fatalf("empty/no-change discovery must not write menu: %s", out)
			}
			sealed := syncVaultBytes(t)
			if bytes.Equal(before, sealed) {
				t.Fatal("apply failed to persist discovery with zero additions")
			}
			syncRun(t, "--apply")
			if f.me.Load() != 3 || f.list.Load() != 2 || !bytes.Equal(sealed, syncVaultBytes(t)) {
				t.Fatalf("zero-addition cache hit rewrote or listed: Me=%d listing=%d", f.me.Load(), f.list.Load())
			}
			syncRun(t, "--refresh", "--apply")
			refreshed := syncVaultBytes(t)
			if f.me.Load() != 4 || f.list.Load() != 3 || bytes.Equal(sealed, refreshed) || providers.OpencodeMenuFromVault() {
				t.Fatal("zero-addition refresh must update discovery, not the menu")
			}
			syncRun(t, "--apply")
			if f.me.Load() != 5 || f.list.Load() != 3 || !bytes.Equal(refreshed, syncVaultBytes(t)) {
				t.Fatal("refreshed snapshot was not reused without writes")
			}
		})
	}
}

func TestOpencodeMenuSyncControlsBeforeHTTP(t *testing.T) {
	f := syncEnv(t)
	if err := providers.SetPins(map[string]string{providers.PinOpencodeProfile: "synctest"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"sync"},
		{"sync", "--profile", "synctest", "--apply", "--dry-run"},
		{"sync", "--profile", "synctest", "unexpected"},
	} {
		before := syncVaultBytes(t)
		if code, _, _ := ocMenu(t, args...); code == 0 || f.me.Load() != 0 || f.list.Load() != 0 || !bytes.Equal(before, syncVaultBytes(t)) {
			t.Fatalf("invalid controls reached HTTP or wrote state: %v, code=%d", args, code)
		}
	}
	if err := providers.SetProviderFields("synctest", map[string]any{"options": map[string]any{"wire": "ai-gateway"}}); err != nil {
		t.Fatal(err)
	}
	before := syncVaultBytes(t)
	if code, _, _ := ocMenu(t, "sync", "--profile", "synctest", "--apply"); code == 0 || f.me.Load() != 0 || f.list.Load() != 0 || !bytes.Equal(before, syncVaultBytes(t)) {
		t.Fatal("gateway profile was not rejected before HTTP/writes")
	}
}

func TestOpencodeMenuSyncFailuresNeverUseStaleOrWrite(t *testing.T) {
	f := syncEnv(t)
	syncRun(t, "--apply")
	before := syncVaultBytes(t)
	f.meCode.Store(401)
	code, stdout, stderr := ocMenu(t, "sync", "--profile", "synctest", "--apply")
	if code == 0 || f.me.Load() != 2 || f.list.Load() != 1 || !bytes.Equal(before, syncVaultBytes(t)) {
		t.Fatalf("revoked auth used cache or wrote state: code=%d Me=%d listing=%d", code, f.me.Load(), f.list.Load())
	}
	if strings.Contains(stdout+stderr, "synthetic-sync-credential") || strings.Contains(stdout+stderr, "private-upstream-body") {
		t.Fatal("identity error leaked sensitive text")
	}
	f.meCode.Store(200)
	f.listCode.Store(503)
	f.listing.Store("private-upstream-body synthetic-sync-credential")
	code, stdout, stderr = ocMenu(t, "sync", "--profile", "synctest", "--refresh", "--apply")
	if code == 0 || f.me.Load() != 3 || f.list.Load() != 2 || !bytes.Equal(before, syncVaultBytes(t)) {
		t.Fatalf("failed refresh used stale data or wrote state: code=%d Me=%d listing=%d", code, f.me.Load(), f.list.Load())
	}
	if strings.Contains(stdout+stderr, "synthetic-sync-credential") || strings.Contains(stdout+stderr, "private-upstream-body") {
		t.Fatal("listing error leaked sensitive text")
	}
}

func TestOpencodeMenuSyncHistoricalFailuresOnlyBlockNewEntries(t *testing.T) {
	f := syncEnv(t)
	existing := []providers.OpencodeModel{{ID: "gpt-5-5", Label: "Keep failed existing", Responses: false}}
	if err := providers.SetOpencodeMenu(existing); err != nil {
		t.Fatal(err)
	}
	s, err := agent.DiscoverServingModels(context.Background(), f.p, false)
	if err != nil {
		t.Fatal(err)
	}
	s.CheckedAt = time.Now().UTC()
	s.Checks = []agent.CheckResult{
		{ID: "gpt-5-5", Status: "fail", HTTP: 400, Error: "private-upstream-body synthetic-sync-credential"},
		{ID: "sync-alpha", Status: "fail", HTTP: 403, Error: "private-upstream-body"},
	}
	if err := agent.SaveServingDiscovery(f.p, s); err != nil {
		t.Fatal(err)
	}
	out := syncRun(t, "--apply")
	got := providers.OpencodeMenu()
	if len(got) != 3 || got[0] != existing[0] || slices.Contains(menuIDs(), "sync-alpha") ||
		!strings.Contains(out, "historical probes") || !strings.Contains(out, "1 failed additions skipped") {
		t.Fatalf("historical checks must block only failed NEW entries: menu=%+v out=%s", got, out)
	}
	if f.me.Load() != 2 || f.list.Load() != 1 || strings.Contains(out, "private-upstream-body") {
		t.Fatal("historical cache was not reused safely")
	}
	if _, err := os.Stat(agent.CheckCachePath()); !os.IsNotExist(err) {
		t.Fatalf("sync touched legacy plaintext check cache: %v", err)
	}
}

func TestOpencodeMenuSyncVerifySealsSafeChecksAndPreservesExisting(t *testing.T) {
	var chat, responses atomic.Int32
	f := syncEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected JSON probe POST")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("probe JSON: %v", err)
		}
		switch r.URL.Path {
		case "/serving-endpoints/chat/completions":
			chat.Add(1)
			if body["model"] != "databricks-gpt-5-5" && body["model"] != "databricks-sync-rejected" {
				t.Errorf("wrong chat model: %v", body["model"])
			}
			http.Error(w, "private-probe-body synthetic-sync-credential", http.StatusForbidden)
		case "/serving-endpoints/responses":
			responses.Add(1)
			if body["model"] != "databricks-gpt-6-astra" {
				t.Errorf("wrong Responses model: %v", body["model"])
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Errorf("unexpected verification route: %s", r.URL.Path)
			http.Error(w, "unexpected path", 404)
		}
	})
	f.listing.Store(`{"endpoints":[
{"name":"databricks-gpt-5-5","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-gpt-6-astra","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-rejected","task":"llm/v1/chat","state":{"ready":"READY"}}]}`)
	existing := []providers.OpencodeModel{
		{ID: "gpt-5-5", Label: "Keep explicit chat flag", Responses: false},
		{ID: "absent-from-discovery", Label: "Keep absent", Responses: true},
	}
	if err := providers.SetOpencodeMenu(existing); err != nil {
		t.Fatal(err)
	}
	before := syncVaultBytes(t)
	preview := syncRun(t, "--verify", "--dry-run")
	if !bytes.Equal(before, syncVaultBytes(t)) || chat.Load() != 4 || responses.Load() != 1 {
		t.Fatal("verification preview must probe without persisting results or menu")
	}
	out := syncRun(t, "--verify", "--apply")
	got := providers.OpencodeMenu()
	if len(got) != 3 || !reflect.DeepEqual(got[:2], existing) || got[2].ID != "gpt-6-astra" || !got[2].Responses ||
		!strings.Contains(out, "1 failed additions skipped") || !strings.Contains(out, "fresh probes") {
		t.Fatalf("verified merge failed: menu=%+v output=%s", got, out)
	}
	if f.me.Load() != 2 || f.list.Load() != 2 || chat.Load() != 8 || responses.Load() != 2 {
		t.Fatalf("wrong verification requests: Me=%d listing=%d chat=%d responses=%d", f.me.Load(), f.list.Load(), chat.Load(), responses.Load())
	}
	snapshot, err := agent.DiscoverServingModels(context.Background(), f.p, false)
	if err != nil || !snapshot.CacheHit || snapshot.CheckedAt.IsZero() || len(snapshot.Checks) != 3 {
		t.Fatalf("sealed verification snapshot missing: %+v err=%v", snapshot, err)
	}
	for _, check := range snapshot.Checks {
		wantStatus, wantHTTP := "fail", http.StatusForbidden
		if check.ID == "gpt-6-astra" {
			wantStatus, wantHTTP = "ok", http.StatusOK
		}
		if check.Status != wantStatus || check.HTTP != wantHTTP || check.Error != "" {
			t.Errorf("unsanitized or incorrect saved check: %+v", check)
		}
	}
	// Inspect decrypted storage too: read-time sanitation alone must not hide a persisted raw body.
	var discoveryKeys []string
	if err := vault.View(func(tx *vault.Tx) error {
		for _, name := range tx.Names(vault.Secrets) {
			if strings.HasPrefix(name, "model-discovery/") {
				discoveryKeys = append(discoveryKeys, name)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(discoveryKeys) != 1 {
		t.Fatalf("discovery records = %d, want one", len(discoveryKeys))
	}
	raw, found, err := vault.ReadSecret(discoveryKeys[0])
	if err != nil || !found {
		t.Fatalf("read sealed snapshot: found=%v err=%v", found, err)
	}
	for _, forbidden := range []string{"private-probe-body", "synthetic-sync-credential"} {
		if strings.Contains(raw+preview+out, forbidden) {
			t.Error("verification persisted or printed private probe diagnostics")
		}
	}
	if _, err := os.Stat(agent.CheckCachePath()); !os.IsNotExist(err) {
		t.Fatalf("verification touched legacy plaintext cache: %v", err)
	}
	sealed := syncVaultBytes(t)
	syncRun(t, "--apply")
	if !bytes.Equal(sealed, syncVaultBytes(t)) || f.list.Load() != 2 || chat.Load() != 8 || responses.Load() != 2 {
		t.Fatal("historical verified hit must not reprobe, relist, or rewrite")
	}
}
