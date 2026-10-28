package databricks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
)

const mintToken = "tok-SECRET-0123456789"

// mintRow seeds a command-auth overlay row "mrow" served by srv (a catalog GET answering status).
func mintRow(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, `{"data":[]}`)
	}))
	t.Cleanup(srv.Close)
	seedOverlay(t, fmt.Sprintf(`{"providers":{"databricks":{"mrow":{"protocol":"anthropic","base_url":%q,`+
		`"auth":{"type":"key","command":"printf %s"}}}}}`, srv.URL, mintToken))
	return srv
}

// stubClipboard puts a pbcopy stub first on PATH that records stdin to the returned file.
func stubClipboard(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	rec := filepath.Join(dir, "clip.out")
	script := "#!/bin/sh\ncat > " + rec + "\n"
	if err := os.WriteFile(filepath.Join(dir, "pbcopy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(clipboard.SetWSLForTest(false))
	return rec
}

// noClipboard leaves only sh on PATH, so no clipboard tool resolves.
func noClipboard(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink("/bin/sh", filepath.Join(dir, "sh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Cleanup(clipboard.SetWSLForTest(false))
}

func noToken(t *testing.T, label, s string) {
	t.Helper()
	if strings.Contains(s, mintToken) {
		t.Errorf("%s leaked the token: %q", label, s)
	}
}

func TestMintResultHidesToken(t *testing.T) {
	mintRow(t, 200)
	res, tok, err := Mint(context.Background(), "mrow")
	if err != nil || tok != mintToken {
		t.Fatalf("tok %q err %v", tok, err)
	}
	if res.Provider != "mrow" || res.AuthType != "key" || res.Len != len(mintToken) {
		t.Errorf("result %+v", res)
	}
	noToken(t, "MintResult", fmt.Sprintf("%+v", res))
	noToken(t, "mintLine", mintLine(res))
}

func TestMintVerbNeverPrintsToken(t *testing.T) {
	mintRow(t, 200)
	code, stdout, stderr := captureOutput(t, func() int { return mint([]string{"--profile", "mrow"}) })
	if code != 0 || !strings.Contains(stdout, "mrow · key · minted · 21 chars") {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	noToken(t, "stdout", stdout)
	noToken(t, "stderr", stderr)
}

func TestMintVerbErrorPathsHideToken(t *testing.T) {
	mintRow(t, 200)
	for _, args := range [][]string{{"nope"}, {"mrow", "--bogus"}} {
		code, stdout, stderr := captureOutput(t, func() int { return mint(args) })
		if code == 0 {
			t.Errorf("%v: exit 0", args)
		}
		noToken(t, "stdout", stdout)
		noToken(t, "stderr", stderr)
	}
	seedOverlay(t, `{"providers":{"databricks":{"mrow":{"protocol":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","command":"echo `+mintToken+`; exit 3"}}}}}`)
	code, stdout, stderr := captureOutput(t, func() int { return mint([]string{"--profile", "mrow", "--copy"}) })
	if code == 0 {
		t.Errorf("failing command: exit 0")
	}
	noToken(t, "stdout", stdout)
	noToken(t, "stderr", stderr)
}

func TestMintDryRunWritesNothing(t *testing.T) {
	mintRow(t, 200)
	rec := stubClipboard(t)
	before := overlayDoc(t)
	code, stdout, stderr := captureOutput(t, func() int { return mint([]string{"--profile", "mrow", "--dry-run", "--copy"}) })
	if code != 0 || !strings.Contains(stdout, "nothing written") {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	noToken(t, "stdout", stdout)
	if _, err := os.Stat(rec); err == nil {
		t.Error("dry-run reached the clipboard")
	}
	if overlayDoc(t) != before {
		t.Error("dry-run changed the overlay")
	}
	if _, ok := providers.TokenExpiry("mrow"); ok {
		t.Error("dry-run recorded a token expiry")
	}
}

func TestMintCopyWritesViaStdinAndHidesToken(t *testing.T) {
	mintRow(t, 200)
	rec := stubClipboard(t)
	code, stdout, stderr := captureOutput(t, func() int { return mint([]string{"--profile", "mrow", "--copy", "--clear-after", "5s"}) })
	got, err := os.ReadFile(rec)
	if err != nil || string(got) != mintToken {
		t.Fatalf("clipboard stub got %q err %v (WriteSecret not called)", got, err)
	}
	// ScheduleClear refuses under test, so the verb reports the failed clear (exit 1) without the token.
	if code != 1 || !strings.Contains(stderr, "timed clear failed") {
		t.Errorf("exit %d stderr %q (ScheduleClear not reached)", code, stderr)
	}
	noToken(t, "stdout", stdout)
	noToken(t, "stderr", stderr)
}

func TestMintCopyNoClipboardPrintsRouterNote(t *testing.T) {
	mintRow(t, 200)
	noClipboard(t)
	code, stdout, stderr := captureOutput(t, func() int { return mint([]string{"--profile", "mrow", "--copy"}) })
	if code != 0 || !strings.Contains(stderr, "the agents already fetch the token through the router") {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "minted") {
		t.Errorf("status line missing: %q", stdout)
	}
	noToken(t, "stdout", stdout)
	noToken(t, "stderr", stderr)
}

func TestSetPATStoresAndVerifies(t *testing.T) {
	mintRow(t, 200)
	status, err := SetPAT(context.Background(), "mrow", "  dapi-NEW  ")
	if err != nil || status != "verified" {
		t.Fatalf("status %q err %v", status, err)
	}
	if v, ok, _ := vault.ReadSecret(providers.SecretName(providers.GroupDatabricks, "mrow", providers.AuthPAT)); !ok || v != "dapi-NEW" {
		t.Errorf("stored %q ok=%v", v, ok)
	}
	if !strings.Contains(overlayDoc(t), providers.AuthPAT) {
		t.Error("authType not switched to pat")
	}
}

func TestSetPATRejectedKeepsOldValue(t *testing.T) {
	mintRow(t, 401)
	name := providers.SecretName(providers.GroupDatabricks, "mrow", providers.AuthPAT)
	if err := secrets.Store(name, "dapi-OLD"); err != nil {
		t.Fatal(err)
	}
	status, err := SetPAT(context.Background(), "mrow", "dapi-BAD")
	if status != "rejected" || err == nil {
		t.Fatalf("status %q err %v", status, err)
	}
	noToken(t, "error", err.Error())
	if strings.Contains(err.Error(), "dapi-BAD") {
		t.Errorf("error leaked the PAT: %v", err)
	}
	if v, ok, _ := vault.ReadSecret(name); !ok || v != "dapi-OLD" {
		t.Errorf("old PAT not restored: %q ok=%v", v, ok)
	}
}

func TestSetPATRejectedWithoutOldDeletesSecret(t *testing.T) {
	mintRow(t, 401)
	if _, err := SetPAT(context.Background(), "mrow", "dapi-BAD"); err == nil {
		t.Fatal("want error")
	}
	if v, ok, _ := vault.ReadSecret(providers.SecretName(providers.GroupDatabricks, "mrow", providers.AuthPAT)); ok {
		t.Errorf("rejected PAT left behind: %q", v)
	}
}

func TestSetPATEmptyErrors(t *testing.T) {
	mintRow(t, 200)
	if _, err := SetPAT(context.Background(), "mrow", "  "); err == nil {
		t.Error("empty PAT accepted")
	}
}

func TestSwitchAuthTypeM2MNeedsClientID(t *testing.T) {
	mintRow(t, 200)
	before := overlayDoc(t)
	p := &providers.Provider{Name: "mrow"}
	if err := switchAuthType(p, providers.AuthM2M, false); err == nil || !strings.Contains(err.Error(), "claude-code vault edit") {
		t.Fatalf("err %v", err)
	}
	if overlayDoc(t) != before {
		t.Error("failed switch wrote the overlay")
	}
	seedOverlay(t, `{"providers":{"databricks":{"mrow":{"protocol":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","command":"true","type":"m2m","clientId":"cid"}}}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, _ = reg.Get("mrow")
	if err := switchAuthType(p, providers.AuthM2M, true); err != nil {
		t.Errorf("with clientId: %v", err)
	}
	if err := switchAuthType(p, "bogus", true); err == nil {
		t.Error("unsupported authType accepted")
	}
}

func TestSwitchAuthTypeDiffAndRollbackHintUnchanged(t *testing.T) {
	seedCLIProfileRow(t)
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { return 0, context.DeadlineExceeded })
	var out strings.Builder
	code, stdout, stderr := captureOutput(t, func() int {
		if err := LoginRow(context.Background(), "dbx-dev", &out); err == nil {
			return 0
		}
		return 1
	})
	if code != 1 || out.Len() != 0 {
		t.Fatalf("exit %d out %q", code, out.String())
	}
	for _, want := range []string{`"type": "oauth"`, "kept custom auth.refreshCommand"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("diff missing %q:\n%s", want, stdout)
		}
	}
	for _, want := range []string{"auth.type of dbx-dev is now oauth (was \"key\")", "providers.prev"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("rollback hint missing %q:\n%s", want, stderr)
		}
	}
}

func TestLoginRowSuccessLineNoRollback(t *testing.T) {
	seedCLIProfileRow(t)
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { return 25 * time.Minute, nil })
	var out strings.Builder
	_, _, stderr := captureOutput(t, func() int {
		if err := LoginRow(context.Background(), "dbx-dev", &out); err != nil {
			t.Error(err)
		}
		return 0
	})
	if out.String() != "logged in · token expires in 25m\n" {
		t.Errorf("out %q", out.String())
	}
	if strings.Contains(stderr, "providers.prev") {
		t.Errorf("rollback hint on success: %q", stderr)
	}
}
