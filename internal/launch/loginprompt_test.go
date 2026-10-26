package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type loginHarness struct {
	calls  *atomic.Int32 // loginRow invocations
	mints  *atomic.Int32 // token endpoint hits
	out    *bytes.Buffer
	server string
}

// loginSetup fakes the TTY, prompt input, token endpoint and loginRow seam.
func loginSetup(t *testing.T, tty bool, input string, loginErr error) loginHarness {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	h := loginHarness{calls: &atomic.Int32{}, mints: &atomic.Int32{}, out: &bytes.Buffer{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mints.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "acc", "expires_in": 3600})
	}))
	t.Cleanup(srv.Close)
	h.server = srv.URL
	oldRow, oldTTY, oldIn, oldOut := loginRow, canPrompt, loginIn, loginOut
	t.Cleanup(func() { loginRow, canPrompt, loginIn, loginOut = oldRow, oldTTY, oldIn, oldOut })
	canPrompt = func() bool { return tty }
	loginIn, loginOut = strings.NewReader(input), h.out
	loginRow = func(_ context.Context, name string, _ io.Writer) error {
		h.calls.Add(1)
		if loginErr != nil {
			return loginErr
		}
		return providers.SealSecret(providers.AuthSecretName(&providers.Provider{Name: name, Auth: providers.Auth{AuthType: providers.AuthU2M}}), "rt")
	}
	return h
}

func u2mRow(name, url string) *providers.Provider {
	return &providers.Provider{Name: name, Kind: providers.DialectAnthropic, BaseURL: url + "/serving-endpoints/anthropic",
		Auth: providers.Auth{Type: providers.AuthBearer, AuthType: providers.AuthU2M, TTL: 1800}}
}

func TestOfferLoginSuccessRetries(t *testing.T) {
	h := loginSetup(t, true, "\n", nil)
	var plan Plan
	offerLogin(context.Background(), Options{}, nil, []*providers.Provider{u2mRow("lp-ok", h.server)}, &plan)
	if h.calls.Load() != 1 || h.mints.Load() != 1 || len(plan.Warnings) != 0 {
		t.Fatalf("login=%d mints=%d warnings=%v", h.calls.Load(), h.mints.Load(), plan.Warnings)
	}
	if !strings.Contains(h.out.String(), "Log in now? [Y/n]") {
		t.Errorf("prompt = %q", h.out.String())
	}
}

func TestOfferLoginFailureWarns(t *testing.T) {
	h := loginSetup(t, true, "y\n", errors.New("browser closed"))
	var plan Plan
	offerLogin(context.Background(), Options{}, nil, []*providers.Provider{u2mRow("lp-fail", h.server)}, &plan)
	if h.calls.Load() != 1 || len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "browser closed") {
		t.Fatalf("login=%d warnings=%v", h.calls.Load(), plan.Warnings)
	}
}

func TestOfferLoginSkippedWhenNotInteractive(t *testing.T) {
	cases := map[string]struct {
		tty  bool
		o    Options
		args []string
	}{
		"non-tty":   {false, Options{}, nil},
		"print-env": {true, Options{PrintEnv: true}, nil},
		"headless":  {true, Options{}, []string{"-p", "hi"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := loginSetup(t, c.tty, "y\n", nil)
			var plan Plan
			offerLogin(context.Background(), c.o, c.args, []*providers.Provider{u2mRow("lp-skip", h.server)}, &plan)
			if h.calls.Load() != 0 || h.out.Len() != 0 || len(plan.Warnings) != 0 {
				t.Fatalf("login=%d out=%q warnings=%v", h.calls.Load(), h.out.String(), plan.Warnings)
			}
		})
	}
}

func TestOfferLoginDecliningWarns(t *testing.T) {
	h := loginSetup(t, true, "n\n", nil)
	var plan Plan
	offerLogin(context.Background(), Options{}, nil, []*providers.Provider{u2mRow("lp-no", h.server)}, &plan)
	if h.calls.Load() != 0 || len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "login skipped") {
		t.Fatalf("login=%d warnings=%v", h.calls.Load(), plan.Warnings)
	}
	if !strings.Contains(h.out.String(), "Log in now?") {
		t.Errorf("expected a prompt, got %q", h.out.String())
	}
}

func TestOfferLoginIgnoresNonU2MAndDedupes(t *testing.T) {
	h := loginSetup(t, true, "y\n", errors.New("x"))
	pat := u2mRow("lp-pat", h.server)
	pat.Auth.AuthType = providers.AuthPAT
	var plan Plan
	offerLogin(context.Background(), Options{}, nil, []*providers.Provider{pat, nil}, &plan)
	if h.calls.Load() != 0 || h.out.Len() != 0 {
		t.Fatalf("non-u2m prompted: login=%d out=%q", h.calls.Load(), h.out.String())
	}
	dup := u2mRow("lp-dup", h.server)
	offerLogin(context.Background(), Options{}, nil, []*providers.Provider{dup, dup}, &plan)
	if h.calls.Load() != 1 {
		t.Errorf("duplicate provider logged in %d times, want 1", h.calls.Load())
	}
}

func TestOfferLoginSharesOneReaderAcrossProviders(t *testing.T) {
	h := loginSetup(t, true, "\nn\n", nil)
	var plan Plan
	ps := []*providers.Provider{u2mRow("lp-a", h.server), u2mRow("lp-b", h.server)}
	offerLogin(context.Background(), Options{}, nil, ps, &plan)
	if h.calls.Load() != 1 || len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "lp-b") {
		t.Fatalf("login=%d warnings=%v", h.calls.Load(), plan.Warnings)
	}
}
