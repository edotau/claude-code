package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func stubToken(t *testing.T, mint func(context.Context, string, int) (string, error)) {
	t.Helper()
	oe, os_, om := tokenEnsure, tokenState, tokenMint
	t.Cleanup(func() { tokenEnsure, tokenState, tokenMint = oe, os_, om })
	tokenEnsure = func(context.Context) (string, error) { return "http://127.0.0.1:1", nil }
	tokenState = func(context.Context) (proxy.State, bool) { return proxy.State{PID: 4242}, true }
	tokenMint = mint
}

func TestGatewayTokenPrintsOnlyDaemonBoundSessionToken(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "CLAUDE_CODE_ROUTER_SECRET", "long-lived-router-secret")
	var gotProfile string
	var gotPID int
	stubToken(t, func(_ context.Context, p string, pid int) (string, error) {
		gotProfile, gotPID = p, pid
		return "cct_abc", nil
	})
	code, stdout, stderr := captureOutput(t, func() int { return cmdGateway([]string{"token", "--profile", "dev"}) })
	if code != 0 || stdout != "cct_abc\n" || stderr != "" || gotProfile != "dev" || gotPID != 4242 {
		t.Fatalf("exit %d stdout %q stderr %q profile %q pid %d", code, stdout, stderr, gotProfile, gotPID)
	}
}

func TestGatewayTokenRejectsBadArgsAndMintFailure(t *testing.T) {
	vaulttest.Use(t)
	stubToken(t, func(context.Context, string, int) (string, error) { return "", errors.New("boom") })
	for _, args := range [][]string{{"token"}, {"token", "extra"}, {"token", "--profile", "dev"}} {
		code, stdout, _ := captureOutput(t, func() int { return cmdGateway(args) })
		if code == 0 || stdout != "" {
			t.Fatalf("%v: exit %d stdout %q", args, code, stdout)
		}
	}
}

func TestDesktopAndTokenHelpFitsEightyColumns(t *testing.T) {
	for _, args := range [][]string{{"configure", "desktop", "--help"}, {"gateway", "token", "--help"}} {
		code, stdout, stderr := captureOutput(t, func() int { return Run(append([]string{"claude-code"}, args...)) })
		if code != 0 || stdout != "" {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
		for line := range strings.SplitSeq(stderr, "\n") {
			if termWidth(line) > 80 {
				t.Errorf("%v: line exceeds 80 columns: %s", args, line)
			}
		}
	}
}
