package cli

import (
	"fmt"
	"github.com/edotau/claude-code/internal/agent"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// qaProfile seeds a hermetic vault with a Databricks row "qa" at host, served-aware routing set to on.
func qaProfile(t *testing.T, host string, on bool) {
	t.Helper()
	cliScratch(t)
	vaulttest.Use(t)
	row := `{"routing":{"servedAware":%t},"providers":{"databricks":{"qa":{"preset":"databricks","host":%q,` +
		`"auth":{"type":"pat"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(row, on, host))
	vaulttest.Secret(t, "DATABRICKS_QA_PAT", "dapi-qa")
}

// statusServer answers every request with code and counts them.
func statusServer(t *testing.T, code int) (*httptest.Server, *int) {
	t.Helper()
	n := new(int)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*n++
		w.WriteHeader(code)
	}))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	return srv, n
}

// switch onto a Databricks profile with no cache lists the workspace once and caches it.
func TestSwitchWarmsMissingServedCache(t *testing.T) {
	srv, got := dbxCLIServer(t)
	qaProfile(t, srv.URL, true)
	code, stdout, stderr := captureOutput(t, func() int { return cmdSwitch([]string{"qa"}) })
	if code != 0 || !strings.Contains(stdout, "profile pinned: qa") {
		t.Fatalf("exit %d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	if len(*got) == 0 {
		t.Fatal("switch made no listing request")
	}
	if s, ok := router.LoadServed("qa"); !ok || len(s.Models) != 3 {
		t.Fatalf("cache after switch = %+v ok=%v, want the 3 listed endpoints", s, ok)
	}
}

// a listing failure prints the warning and still exits 0 with the pin written.
func TestSwitchRefreshFailurePrintsAndExitsZero(t *testing.T) {
	srv, n := statusServer(t, http.StatusInternalServerError)
	qaProfile(t, srv.URL, true)
	code, stdout, stderr := captureOutput(t, func() int { return cmdSwitch([]string{"qa"}) })
	if code != 0 || !strings.Contains(stdout, "profile pinned: qa") || !strings.Contains(stdout, "listing refresh failed") {
		t.Fatalf("exit %d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	if *n == 0 {
		t.Error("the refresh never reached the server")
	}
}

// --dry-run previews the pin and touches neither the vault nor the network.
func TestSwitchDryRunMakesNoRequests(t *testing.T) {
	srv, got := dbxCLIServer(t)
	qaProfile(t, srv.URL, true)
	if code, _, stderr := captureOutput(t, func() int { return cmdSwitch([]string{"qa", "--dry-run"}) }); code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, stderr)
	}
	if len(*got) != 0 {
		t.Fatalf("dry-run made requests: %v", *got)
	}
	if _, ok := router.LoadServed("qa"); ok {
		t.Error("dry-run wrote a cache")
	}
}

// switch off (the default): the pin lands and no request is made.
func TestSwitchWithSwitchOffMakesNoRequests(t *testing.T) {
	srv, got := dbxCLIServer(t)
	qaProfile(t, srv.URL, false)
	if code, out, _ := captureOutput(t, func() int { return cmdSwitch([]string{"qa"}) }); code != 0 || !strings.Contains(out, "pinned") {
		t.Fatalf("exit %d out=%q", code, out)
	}
	if len(*got) != 0 {
		t.Fatalf("switch off still made requests: %v", *got)
	}
}

// switch moves claude and each agent that is on, and leaves an off agent off (dry-run says so).
func TestSwitchKeepsOffAgentsOff(t *testing.T) {
	srv, _ := dbxCLIServer(t)
	qaProfile(t, srv.URL, false)
	if err := agent.SetAgentRecord("codex", agent.AgentRecord{Profile: "chatgpt"}); err != nil {
		t.Fatal(err)
	}
	_, out, _ := captureOutput(t, func() int { return cmdSwitch([]string{"qa", "--dry-run"}) })
	if !strings.Contains(out, "codex.profile: chatgpt → qa") || !strings.Contains(out, "copilot: off (kept)") {
		t.Fatalf("dry-run = %q", out)
	}
	if code, _, stderr := captureOutput(t, func() int { return cmdSwitch([]string{"qa"}) }); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, stderr)
	}
	recs := agent.AgentRecords()
	if recs["claude"].Profile != "qa" || recs["codex"].Profile != "qa" || recs["copilot"].Profile != "" {
		t.Fatalf("profiles claude=%q codex=%q copilot=%q, want qa qa and copilot off",
			recs["claude"].Profile, recs["codex"].Profile, recs["copilot"].Profile)
	}
}
