package gateway

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// tokenDaemon answers only health probes and token requests, without starting a daemon.
func tokenDaemon(t *testing.T, mint func(http.ResponseWriter, proxy.SessionRequest)) {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Secret(t, "CLAUDE_CODE_ROUTER_SECRET", "long-lived-router-secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set(proxy.PIDHeader, strconv.Itoa(os.Getpid()))
			return
		}
		if r.URL.Path != proxy.SessionPath || r.Header.Get("X-Api-Key") != "long-lived-router-secret" {
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var req proxy.SessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mint(w, req)
	}))
	t.Cleanup(srv.Close)
	cleanup, err := proxy.WriteState(proxy.State{PID: os.Getpid(), Port: srv.Listener.Addr().(*net.TCPAddr).Port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
}

func TestGatewayTokenPrintsOnlyDaemonBoundSessionToken(t *testing.T) {
	var gotProfile string
	var gotPID int
	tokenDaemon(t, func(w http.ResponseWriter, req proxy.SessionRequest) {
		gotProfile, gotPID = req.Profile, req.PID
		json.NewEncoder(w).Encode(proxy.SessionResponse{Token: "cct_abc"})
	})
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"token", "--profile", "dev"}) })
	if code != 0 || stdout != "cct_abc\n" || stderr != "" || gotProfile != "dev" || gotPID != os.Getpid() {
		t.Fatalf("exit %d stdout %q stderr %q profile %q pid %d", code, stdout, stderr, gotProfile, gotPID)
	}
}

func TestGatewayTokenRejectsBadArgsAndMintFailure(t *testing.T) {
	tokenDaemon(t, func(w http.ResponseWriter, _ proxy.SessionRequest) { w.WriteHeader(http.StatusInternalServerError) })
	for _, args := range [][]string{{"token"}, {"token", "extra"}, {"token", "--profile", "dev"}} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run(args) })
		if code == 0 || stdout != "" || !strings.Contains(stderr, "gateway token:") {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

// TestPrintTokenWriteFailureNamesCause: a failed stdout write exits 1 with a stderr message.
func TestPrintTokenWriteFailureNamesCause(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no /dev/full: %v", err)
	}
	defer full.Close()
	r, w, _ := os.Pipe()
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = full, w
	t.Cleanup(func() { os.Stdout, os.Stderr = origOut, origErr })
	code := printToken("cct_abc")
	w.Close()
	msg, _ := io.ReadAll(r)
	if code != 1 || !strings.Contains(string(msg), "gateway token: write stdout") {
		t.Fatalf("want exit 1 with a 'gateway token: write stdout' message, got %d %q", code, msg)
	}
}
