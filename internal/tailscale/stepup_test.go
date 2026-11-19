package tailscale

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

func TestStepUpRefusesNonTTYBeforeAnyCall(t *testing.T) {
	prevTTY, prevRe := stdioIsTTY, reauthRun
	t.Cleanup(func() { stdioIsTTY, reauthRun = prevTTY, prevRe })
	stdioIsTTY = func() bool { return false }
	reauthRun = func(context.Context, io.Writer) error { t.Error("re-auth ran"); return nil }
	err := stepUp(context.Background(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "own terminal") {
		t.Fatalf("stepUp err = %v, want an own-terminal refusal", err)
	}
}

func TestStepUpRefusesChangedLoginName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	names := []string{"owner@example.com", "other@example.com"}
	calls := 0
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := names[min(calls, 1)]
		calls++
		io.WriteString(w, `{"BackendState":"Running","Self":{"UserID":7,"DNSName":"n.ts.net."},"User":{"7":{"LoginName":"`+name+`"}}}`)
	}))
	t.Cleanup(StubLocalAPI(sock))
	prevTTY, prevRe := stdioIsTTY, reauthRun
	t.Cleanup(func() { stdioIsTTY, reauthRun = prevTTY, prevRe })
	stdioIsTTY = func() bool { return true }
	reauthRun = func(context.Context, io.Writer) error { return nil }
	t.Setenv("SSH_CONNECTION", "")
	err := stepUp(context.Background(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "different user") {
		t.Fatalf("stepUp err = %v, want a different-user refusal", err)
	}
}
