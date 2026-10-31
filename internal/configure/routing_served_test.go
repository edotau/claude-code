package configure

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// servedHarness seeds a hermetic vault with a Databricks row "qa" (served-aware routing set to on) pinned as the
// session profile, and a TLS listing server; it returns the request counter.
func servedHarness(t *testing.T, on bool) *int {
	t.Helper()
	vaulttest.Use(t)
	var n int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		_, _ = w.Write([]byte(`{"endpoints":[{"name":"m-a","task":"llm/v1/chat","state":{"ready":"READY"}}]}`))
	}))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	row := `{"routing":{"servedAware":%t},"providers":{"databricks":{"qa":{"preset":"databricks","host":%q,` +
		`"auth":{"type":"pat"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(row, on, srv.URL))
	vaulttest.Secret(t, "DATABRICKS_QA_PAT", "dapi-qa")
	return &n
}

func cached(t *testing.T) bool {
	t.Helper()
	_, ok := agent.LoadServed("qa")
	return ok
}

// the switch flipping off -> on warms the cache even though no pin changed.
func TestWarmServedOnFlipOn(t *testing.T) {
	n := servedHarness(t, true)
	if err := providers.SetPins(map[string]string{providers.PinProfile: "qa"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warmServedOnChange(&out, false, agent.AgentPins(), false)
	if *n == 0 || !cached(t) {
		t.Fatalf("flip-on made %d requests, cached=%v, want a warmed cache", *n, cached(t))
	}
}

// a pin change with the switch already on warms; the same pins with it already on do not.
func TestWarmServedOnPinChangeOnly(t *testing.T) {
	n := servedHarness(t, true)
	before := agent.AgentPins()
	var out bytes.Buffer
	warmServedOnChange(&out, false, before, true)
	if *n != 0 {
		t.Fatalf("unchanged pins and switch warmed: %d requests", *n)
	}
	if err := providers.SetPins(map[string]string{providers.PinProfile: "qa"}); err != nil {
		t.Fatal(err)
	}
	warmServedOnChange(&out, false, before, true)
	if *n == 0 || !cached(t) {
		t.Fatalf("pin change made %d requests, cached=%v, want a warmed cache", *n, cached(t))
	}
}

// with the switch off, even a pin change makes no request.
func TestWarmServedSwitchOffNeverWarms(t *testing.T) {
	n := servedHarness(t, false)
	before := agent.AgentPins()
	if err := providers.SetPins(map[string]string{providers.PinProfile: "qa"}); err != nil {
		t.Fatal(err)
	}
	warmServedOnChange(&bytes.Buffer{}, false, before, false)
	if *n != 0 || cached(t) {
		t.Fatalf("switch off: %d requests, cached=%v", *n, cached(t))
	}
}

// --check and --dry-run never warm, whatever changed.
func TestWarmServedSkippedForCheckAndDryRun(t *testing.T) {
	n := servedHarness(t, true)
	before := agent.AgentPins()
	if err := providers.SetPins(map[string]string{providers.PinProfile: "qa"}); err != nil {
		t.Fatal(err)
	}
	warmServedOnChange(&bytes.Buffer{}, true, before, false)
	if *n != 0 || cached(t) {
		t.Fatalf("skip=true: %d requests, cached=%v", *n, cached(t))
	}
}

// Run wires the warm: a non-interactive provider pin change with the switch on warms; --dry-run does not.
func TestRunProviderPinChangeWarmsUnlessDryRun(t *testing.T) {
	n := servedHarness(t, true)
	var out bytes.Buffer
	if rc := Run(Options{Provider: "qa", DryRun: true, In: strings.NewReader(""), Out: &out}, "provider"); rc != 0 || *n != 0 {
		t.Fatalf("dry-run: rc=%d requests=%d out=%s", rc, *n, out.String())
	}
	Run(Options{Provider: "qa", Check: true, In: strings.NewReader(""), Out: &out}, "provider")
	if *n != 0 {
		t.Fatalf("--check warmed: %d requests", *n)
	}
	if rc := Run(Options{Provider: "qa", In: strings.NewReader(""), Out: &out}, "provider"); rc != 0 {
		t.Fatalf("rc=%d out=%s", rc, out.String())
	}
	if providers.Pin(providers.PinProfile) != "qa" || *n == 0 || !cached(t) {
		t.Fatalf("pin=%q requests=%d cached=%v out=%s", providers.Pin(providers.PinProfile), *n, cached(t), out.String())
	}
}
