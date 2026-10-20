package configure

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestStageRejectsInvalidValueWithoutMutation(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "replacement"}[staged], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			const name = "CONFIGURE_STAGE_INVALID"
			t.Setenv(name, "original")
			d, _ := newDialog("")
			d.pending = map[string]stagedKey{}
			if staged && !d.stage(name, "already-staged", true) {
				t.Fatal("valid stage failed")
			}
			before, seen := d.pending[name]
			env := os.Getenv(name)
			if d.stage(name, "invalid\x00candidate", false) {
				t.Fatal("stage accepted a NUL value")
			}
			if got, ok := d.pending[name]; ok != seen || got != before {
				t.Errorf("pending entry changed: got %+v, present=%v; want %+v, present=%v", got, ok, before, seen)
			}
			if got := os.Getenv(name); got != env {
				t.Errorf("environment = %q, want %q", got, env)
			}
			discardKeys(d)
			if got := os.Getenv(name); got != "original" {
				t.Errorf("discard restored %q, want original", got)
			}
		})
	}
}

func TestStoreValueRejectsInvalidStagingWithoutSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const name = "CONFIGURE_STORE_INVALID"
	t.Setenv(name, "original")
	d, out := newDialog("")
	d.pending = map[string]stagedKey{}
	if storeValue(d, name, "invalid\x00candidate", false) {
		t.Fatal("storeValue reported success for a NUL value")
	}
	if len(d.pending) != 0 || os.Getenv(name) != "original" {
		t.Fatalf("failed store mutated pending or environment: pending=%v, env=%q", d.pending, os.Getenv(name))
	}
	if strings.Contains(out.String(), "staged ") || strings.Contains(out.String(), "Submit saves") {
		t.Errorf("failed store reported staged success: %q", out.String())
	}
}

func TestProbeValueRejectsInvalidCandidateBeforeRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const name = "CONFIGURE_PROBE_INVALID"
	const candidate = "private-invalid\x00candidate"
	t.Setenv(name, "prior-good-credential")
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"data":[]}`)); err != nil {
			t.Errorf("write catalog: %v", err)
		}
	}))
	defer srv.Close()
	p := &providers.Provider{
		Name: "invalid-candidate", Kind: providers.DialectAnthropic, BaseURL: srv.URL,
		Auth: providers.Auth{Type: providers.AuthAPIKey, Env: name},
	}
	status, detail := probeValue(p, name, candidate)
	if status != "unreachable" || detail == "" {
		t.Errorf("probeValue = (%q, %q), want unreachable with an error detail", status, detail)
	}
	for _, raw := range []string{candidate, "private-invalid", "prior-good-credential"} {
		if strings.Contains(detail, raw) {
			t.Errorf("probe detail leaked a credential: %q", detail)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("invalid candidate made %d HTTP requests with a prior credential", got)
	}
	if got := os.Getenv(name); got != "prior-good-credential" {
		t.Errorf("probe changed original credential to %q", got)
	}
}
