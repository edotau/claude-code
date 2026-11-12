package menu

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// tsPage is a hermetic tailscale page: numbered shell fed input, every action seam failing unless a test swaps it.
func tsPage(t *testing.T, input string, admin bool) (*dialog, *bytes.Buffer) {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("PATH", t.TempDir())
	t.Cleanup(tailscale.StubMacAppCLI(""))
	for _, k := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY"} {
		t.Setenv(k, "")
	}
	if admin {
		t.Setenv("TAILSCALE_API_KEY", "tskey-api-test")
	}
	resetTailscaleStatus(t)
	sh, l, m, e, v := tsShell, tsLogin, tsMint, tsExitNode, tsValidate
	t.Cleanup(func() { tsShell, tsLogin, tsMint, tsExitNode, tsValidate = sh, l, m, e, v })
	tsValidate = func(context.Context, string, string) error { t.Error("validate ran"); return nil }
	var out bytes.Buffer
	tsShell = func(*dialog) *shell { return &shell{Out: &out, In: bufio.NewReader(strings.NewReader(input))} }
	tsLogin = func(context.Context, io.Reader, io.Writer, io.Writer) error { t.Error("login ran"); return nil }
	tsMint = func(context.Context, tailscale.KeyOptions) (tailscale.AuthKey, error) {
		t.Error("mint ran")
		return tailscale.AuthKey{}, nil
	}
	tsExitNode = func(_ context.Context, mode string) (string, error) {
		t.Error("exit node ran: " + mode)
		return "", nil
	}
	return &dialog{out: &out}, &out
}

// tsConnected stubs Status as a Running node named dns with the given exit-node state.
func tsConnected(dns string, advertised, approved bool) {
	tsStatusFetch = func(context.Context) (tailscale.Local, error) {
		return tailscale.Local{BackendState: "Running", DNSName: dns, ExitNodeOption: advertised,
			ExitNodeApproved: approved}, nil
	}
}

// Every row the page and its exit-node picker draw is one line that fits an 80-column panel, in both states.
func TestTailscalePageRowsFitEightyColumns(t *testing.T) {
	_, out := tsPage(t, "", false) // not connected: the sign-in row shows
	runTailscalePage(&dialog{out: out})
	rows := numberedRows(out.String())
	if len(rows) != 5 || rows[1][0] != "sign in" {
		t.Fatalf("not-connected rows = %q, want 5 with sign in", rows)
	}
	assertFitsEighty(t, "tailscale page (not connected)", append(rows, panelBack))

	_, out = tsPage(t, "4\n", true) // connected, long host, admin key: open the exit-node picker
	tsConnected("a-long-hostname-01.tail1234.ts.net.", true, true)
	runTailscalePage(&dialog{out: out})
	rows = numberedRows(out.String())
	if len(rows) != 4+3+4 || slices.ContainsFunc(rows, func(r [2]string) bool { return r[0] == "sign in" }) {
		t.Fatalf("connected rows = %q, want page, on/off/approve, page again; no sign in", rows)
	}
	assertFitsEighty(t, "tailscale page (connected)", append(rows, panelBack))
}

// fakeTailscaleLogin puts a `tailscale` on PATH that logs its argv, answers NeedsLogin until `login` ran, then Running.
func fakeTailscaleLogin(t *testing.T) (argvLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	argvLog, done := filepath.Join(dir, "argv"), filepath.Join(dir, "done")
	script := "#!/bin/sh\necho \"$@\" >> " + argvLog + "\n" +
		"if [ \"$1\" = login ]; then : > " + done + "; exit 0; fi\n" +
		"state=NeedsLogin; [ -f " + done + " ] && state=Running\n" +
		`echo '{"BackendState":"'$state'","Self":{"DNSName":"box.tail1234.ts.net.","AllowedIPs":[]},"Peer":{}}'` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return argvLog
}

// Sign in runs `tailscale login` (not up), reports the machine name, and the sign-in row is gone once connected.
func TestTailscalePageSignInRunsLogin(t *testing.T) {
	d, out := tsPage(t, "2\n", false)
	tsLogin = tailscale.Login
	argvLog := fakeTailscaleLogin(t)
	runTailscalePage(d)
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSpace(string(b)), "\n")
	if !slices.Contains(argv, "login") || strings.Contains(string(b), "up") {
		t.Errorf("tailscale argv = %q, want `login` and no `up`", argv)
	}
	got := out.String()
	_, after, ok := strings.Cut(got, "tailscale: signed in as box.tail1234.ts.net\n")
	if !ok || strings.Contains(after, "sign in") {
		t.Errorf("want the signed-in line and no sign-in row after it:\n%s", got)
	}
}

// Auth key mints with the answered options (90d default) and seals the key once shown as TAILSCALE_AUTHKEY.
func TestTailscalePageAuthKeyMintsAndSeals(t *testing.T) {
	d, out := tsPage(t, "3\ny\n\ntag:ci\n", true)
	tsConnected("box.tail1234.ts.net.", false, false)
	var got []tailscale.KeyOptions
	tsMint = func(_ context.Context, o tailscale.KeyOptions) (tailscale.AuthKey, error) {
		got = append(got, o)
		return tailscale.AuthKey{Key: "tskey-auth-PAGE0123456789"}, nil
	}
	runTailscalePage(d)
	want := tailscale.KeyOptions{Expiry: "90d", Tags: "tag:ci", Reusable: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("mint options = %+v, want one call with %+v", got, want)
	}
	if n := strings.Count(out.String(), "tskey-auth-PAGE0123456789"); n != 1 {
		t.Errorf("key printed %d times, want once:\n%s", n, out)
	}
	if v, ok, err := vault.ReadSecret("TAILSCALE_AUTHKEY"); err != nil || !ok || v != "tskey-auth-PAGE0123456789" {
		t.Errorf("vault TAILSCALE_AUTHKEY = %q, %v, %v; want the minted key", v, ok, err)
	}
}

// Exit node on, off and approve each call the exit-node seam with that mode, in pick order.
func TestTailscalePageExitNodeActions(t *testing.T) {
	d, out := tsPage(t, "4\n1\n4\n2\n4\n3\n", true)
	tsConnected("box.tail1234.ts.net.", false, false)
	var modes []string
	tsExitNode = func(_ context.Context, mode string) (string, error) {
		modes = append(modes, mode)
		return "tailscale: did " + mode, nil
	}
	runTailscalePage(d)
	if !slices.Equal(modes, []string{"on", "off", "approve"}) {
		t.Errorf("exit-node modes = %q, want on, off, approve", modes)
	}
	if !strings.Contains(out.String(), "tailscale: did approve") {
		t.Errorf("outcome line missing:\n%s", out)
	}
}

// A piped (non-interactive) tailscale row prints runRemote's report and runs no action or picker.
func TestTailscaleRowPipedPrintsReportOnly(t *testing.T) {
	_, out := tsPage(t, "", false)
	tsShell = func(*dialog) *shell { t.Error("page opened"); return &shell{} }
	tsConnected("box.tail1234.ts.net.", false, false)
	tailscaleRow().Run(&dialog{o: Options{Interactive: true, In: strings.NewReader("")}, out: out})
	got := out.String()
	for _, want := range []string{"tailscale   running (box.tail1234.ts.net)", "admin api   credential absent"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
}
