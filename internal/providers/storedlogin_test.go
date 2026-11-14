package providers

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
)

// handSet writes record under p's login secret the way `vault set` does: plain vault.Store, no seal time.
func handSet(t *testing.T, p *Provider, record string) {
	t.Helper()
	if err := vault.Store(AuthSecretName(p), record); err != nil {
		t.Fatal(err)
	}
}

// deadRig is a login rig whose stored login was refused once, leaving a mark.
func deadRig(t *testing.T, name string) *loginRig {
	t.Helper()
	r := newLoginRig(t, name)
	if _, err := Credential(context.Background(), r.p); err == nil {
		t.Fatal("the dead login minted a token")
	}
	if _, dead := LoginStatus(r.p); !dead {
		t.Fatal("no mark after a refused login")
	}
	return r
}

// A hand-written login that bypassed the verify hook is tried once on the next request, then sealed.
func TestHandSetLoginReplacingAMarkedOneIsTried(t *testing.T) {
	r := deadRig(t, "hand-bypass")
	handSet(t, r.p, `{"refresh":"refresh-2"}`)
	r.mode.Store("ok")
	if _, err := Credential(context.Background(), r.p); err != nil || r.hits.Load() != 2 {
		t.Fatalf("Credential after a hand-set login: err %v, hits %d; want one refresh with the new value", err, r.hits.Load())
	}
	if ov, _ := readOAuth(r.p); ov.Sealed == 0 || ov.Refresh != "refresh-2" {
		t.Errorf("record not sealed with a seal time and the hand-set refresh: sealed %d", ov.Sealed)
	}
	if _, dead := LoginStatus(r.p); dead {
		t.Error("the mark outlived a working hand-set login")
	}
}

// A hand-set login the workspace refuses is tried once, not on every request.
func TestHandSetLoginRefusedIsTriedOnceAndRemarked(t *testing.T) {
	r := deadRig(t, "hand-refused")
	handSet(t, r.p, `{"refresh":"refresh-2"}`)
	for range 3 {
		_, _ = Credential(context.Background(), r.p)
	}
	if r.hits.Load() != 2 {
		t.Errorf("token endpoint hits = %d; want 2 (the first failure, then one try of the new value)", r.hits.Load())
	}
	if _, dead := LoginStatus(r.p); !dead {
		t.Error("a refused hand-set login left no mark")
	}
}

func TestVerifyStoredLoginSealsAWorkingLogin(t *testing.T) {
	r := deadRig(t, "verify-ok")
	handSet(t, r.p, `{"refresh":"refresh-2"}`)
	r.mode.Store("ok")
	line := VerifyStoredLogin(context.Background(), r.p)
	if want := "verify-ok: stored login verified and sealed · token expires in "; !strings.HasPrefix(line, want) {
		t.Fatalf("line = %q; want prefix %q", line, want)
	}
	if ov, _ := readOAuth(r.p); ov.Sealed == 0 || ov.Access == "" {
		t.Errorf("record not sealed: seal time %d, access cached %v", ov.Sealed, ov.Access != "")
	}
	if _, err := os.Stat(cacheRecordPath("login-state", r.p.Name)); !os.IsNotExist(err) {
		t.Errorf("mark file after a verified login: %v; want it deleted", err)
	}
	if _, err := Credential(context.Background(), r.p); err != nil || r.hits.Load() != 2 {
		t.Errorf("next request: err %v, hits %d; want the sealed login served with no further refresh", err, r.hits.Load())
	}
}

func TestVerifyStoredLoginKeepsARefusedLoginAndNamesTheCommand(t *testing.T) {
	r := deadRig(t, "verify-refused")
	handSet(t, r.p, `{"refresh":"secret-refresh-2"}`)
	line := VerifyStoredLogin(context.Background(), r.p)
	if !strings.Contains(line, "rejected") || !strings.Contains(line, "`"+LoginCommand(r.p.Name)+"`") || strings.Contains(line, "secret-refresh-2") {
		t.Errorf("line = %q; want the rejection, the sign-in command and no token", line)
	}
	if ov, _ := readOAuth(r.p); ov.Refresh != "secret-refresh-2" {
		t.Error("a refused login must stay stored")
	}
	if _, dead := LoginStatus(r.p); !dead {
		t.Error("a refused login left no mark")
	}
}

func TestVerifyStoredLoginOnANetworkErrorClearsTheMark(t *testing.T) {
	r := deadRig(t, "verify-down")
	handSet(t, r.p, `{"refresh":"refresh-2"}`)
	r.mode.Store("down")
	if line := VerifyStoredLogin(context.Background(), r.p); !strings.Contains(line, "verification skipped") {
		t.Errorf("line = %q; want it to say verification was skipped", line)
	}
	if _, dead := LoginStatus(r.p); dead {
		t.Fatal("the mark survived a skipped verification")
	}
	r.mode.Store("ok")
	hits := r.hits.Load()
	if _, err := Credential(context.Background(), r.p); err != nil || r.hits.Load() != hits+1 {
		t.Errorf("next request: err %v, hits %d -> %d; want exactly one try", err, hits, r.hits.Load())
	}
}

func TestLoginSecretProviderAndRecordShape(t *testing.T) {
	r := newLoginRig(t, "shape")
	if LoginSecretProvider("SOME_API_KEY") != nil || LoginSecretProvider("NOT_A_ROW_OAUTH") != nil {
		t.Error("a non-login name matched a provider")
	}
	for _, bad := range []string{"bare-refresh-token", `{"access":"a"}`, `["x"]`} {
		if err := CheckLoginRecord(AuthSecretName(r.p), bad); err == nil || strings.Contains(err.Error(), bad) {
			t.Errorf("CheckLoginRecord(%q) = %v; want a refusal naming the shape, not the value", bad, err)
		}
	}
	if err := CheckLoginRecord("N", ` {"refresh":"r","access":"a","sealed":5}`); err != nil {
		t.Errorf("a real record was refused: %v", err)
	}
}
