package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// dbxGeminiSetup is a Databricks serving row whose derived gemini route lands on a recording upstream.
func dbxGeminiSetup(t *testing.T) (*rawUp, string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newRawUp(t, okRaw)
	t.Setenv("DBX_KEY", "dbx-token")
	row := `{"preset":"databricks","host":"https://example.cloud.databricks.com","base_url":"` + up.URL +
		`/serving-endpoints/anthropic","auth":{"type":"key","env":"DBX_KEY"}}`
	srv := setup(t, `{"default":"dbx","providers":{"databricks":{"dbx":`+row+`}}}`)
	return up, srv.URL + "/p/dbx/gemini/"
}

// agy's call reaches /serving-endpoints/gemini with the wire model, alt=sse only, fresh token, thoughts-off fixed.
func TestDatabricksGeminiRoute(t *testing.T) {
	up, base := dbxGeminiSetup(t)
	body := `{"contents":[],"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":0}}}`
	resp := ptDo(t, http.MethodPost, base+"v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse&key=x", body, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	g := up.got()
	if len(g) != 1 {
		t.Fatalf("upstream hits %d", len(g))
	}
	if want := "/serving-endpoints/gemini/v1beta/models/databricks-gemini-3-8-flash:streamGenerateContent?alt=sse"; g[0].uri != want {
		t.Errorf("uri = %s, want %s", g[0].uri, want)
	}
	if strings.Contains(g[0].body, "includeThoughts") || !strings.Contains(g[0].body, `"thinkingBudget":0`) {
		t.Errorf("body = %s", g[0].body)
	}
	if a := g[0].hdr.Get("Authorization"); a != "Bearer dbx-token" {
		t.Errorf("authorization = %q", a)
	}
}

// Regression: an encoded or literal climb, or any call but (stream)GenerateContent, never reaches the workspace.
func TestDatabricksGeminiRouteRejectsTraversal(t *testing.T) {
	up, base := dbxGeminiSetup(t)
	for _, rest := range []string{
		"v1beta/models/%2e%2e/%2e%2e/api/2.0/secrets:generateContent", "v1beta/../../api/2.0/clusters/list",
		"v1beta/models/m%252e:generateContent", "v1beta//models/m:generateContent", "v1beta/models/m:countTokens",
		"v1beta/models/m%5c:generateContent",
	} {
		resp := ptDo(t, http.MethodPost, base+rest, `{}`, nil)
		if resp.StatusCode < 400 {
			t.Errorf("%s: status %d", rest, resp.StatusCode)
		}
	}
	if g := up.got(); len(g) != 0 {
		t.Fatalf("upstream reached: %v", g[0].uri)
	}
}

// Regression: every passthrough dialect refuses a dot-segment tail before building the upstream URL.
func TestPassthroughRejectsDotSegments(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newRawUp(t, okRaw)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	for _, rest := range []string{"openai/%2e%2e/admin", "gemini/v1beta/%2e%2e/x", "responses/a%252e%252e/b"} {
		if resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/"+rest, `{}`, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", rest, resp.StatusCode)
		}
	}
	if g := up.got(); len(g) != 0 {
		t.Fatalf("upstream reached: %v", g[0].uri)
	}
}
