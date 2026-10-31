package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/usage"
)

// The embedded copies must be byte-identical to the extension's modules; fix drift with `go generate ./internal/network/web`.
func TestUsageAssetsMatchExtensionSource(t *testing.T) {
	for _, name := range []string{"format", "pricing", "usage-model", "usage-view"} {
		src, err := os.ReadFile(filepath.Join("..", "..", "..", "vscode", "claude-code-agents", "lib", name+".js"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := assetsFS.ReadFile("assets/usage/" + name + ".js")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(src, got) {
			t.Errorf("assets/usage/%s.js differs from vscode/claude-code-agents/lib/%s.js; run go generate", name, name)
		}
	}
}

// /api/usage returns the masked JSON of usage.Report for the asked window, cached for usageTTL.
func TestUsageEndpointServesReportJSONAndCaches(t *testing.T) {
	calls, now := 0, time.Now()
	orig := usageReport
	usageReport = func(_, _ string, days int, _ bool, _ io.Writer) (*usage.Document, error) {
		calls++
		return &usage.Document{WindowDays: days}, nil
	}
	t.Cleanup(func() { usageReport = orig })
	s := newTestServer(t, Options{}, nil)
	s.now = func() time.Time { return now }

	var want bytes.Buffer
	if err := helptext.WriteJSON(&want, &usage.Document{WindowDays: 7}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		w := do(s, newReq(t, "GET", "/api/usage?days=7", "127.0.0.1:7777"))
		var got, wantTree any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(want.Bytes(), &wantTree); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusOK || !reflect.DeepEqual(got, wantTree) {
			t.Fatalf("call %d: %d %q, want 200 %q", i, w.Code, w.Body.String(), want.String())
		}
	}
	if calls != 1 {
		t.Fatalf("report built %d times for two requests inside the TTL, want 1", calls)
	}
	now = now.Add(usageTTL + time.Second)
	do(s, newReq(t, "GET", "/api/usage?days=7", "127.0.0.1:7777"))
	if calls != 2 {
		t.Fatalf("report built %d times after the TTL, want 2", calls)
	}
	for target, host := range map[string]string{"/api/usage?days=0": "127.0.0.1:7777", "/api/usage": "evil.example"} {
		if w := do(s, newReq(t, "GET", target, host)); w.Code == http.StatusOK {
			t.Errorf("GET %s (Host %s) = 200, want a refusal", target, host)
		}
	}
}

// The served body masks emails and token-shaped strings, and the cache holds the masked bytes.
func TestUsageEndpointMasksEmailAndTokens(t *testing.T) {
	const email, token = "eric@gmail.com", "sk-ant-oat01-abcdefghijklmnop"
	var doc usage.Document
	fixture := `{"providers":[{"provider":"claude","account":"` + email + `","note":"token ` + token + ` expired"}],"window_days":7}`
	if err := json.Unmarshal([]byte(fixture), &doc); err != nil {
		t.Fatal(err)
	}
	orig := usageReport
	usageReport = func(_, _ string, _ int, _ bool, _ io.Writer) (*usage.Document, error) { return &doc, nil }
	t.Cleanup(func() { usageReport = orig })
	s := newTestServer(t, Options{}, nil)

	for i := 0; i < 2; i++ { // second call is served from the cache
		body := do(s, newReq(t, "GET", "/api/usage?days=7", "127.0.0.1:7777")).Body.String()
		if strings.Contains(body, email) || strings.Contains(body, token) || !strings.Contains(body, "e…@gmail.com") {
			t.Fatalf("call %d body not masked: %s", i, body)
		}
	}
}
