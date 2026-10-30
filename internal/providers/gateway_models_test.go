package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGatewayModelsPagesAndMarksGatewayRows(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ai-gateway/anthropic/v1/models" {
			writeJSON(w, 404, `{}`)
			return
		}
		if r.URL.Query().Get("after_id") == "" {
			writeJSON(w, 200, `{"data":[{"id":"system.ai.claude-opus-5","display_name":"Claude Opus 5"}],"has_more":true,"last_id":"system.ai.claude-opus-5"}`)
			return
		}
		writeJSON(w, 200, `{"data":[{"id":"system.ai.claude-haiku-5-5","display_name":""}],"has_more":false}`)
	}))
	t.Cleanup(srv.Close)
	trustTestTLS(t, srv)
	got, err := GatewayModels(context.Background(), liveDbxProvider(t, srv.URL, "tok"))
	if err != nil || len(got) != 2 {
		t.Fatalf("GatewayModels = %+v, %v; want 2 rows", got, err)
	}
	if got[0].Name != "Claude Opus 5" || !got[0].Gateway || got[1].Name == "" || !got[1].Gateway {
		t.Errorf("rows = %+v", got)
	}
}

func TestGatewayModelsNonDatabricksAndMissingEndpoint(t *testing.T) {
	if got, err := GatewayModels(context.Background(), &Provider{Name: "x"}); got != nil || err != nil {
		t.Errorf("non-databricks = %v, %v; want nil, nil", got, err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 404, `{}`) }))
	t.Cleanup(srv.Close)
	trustTestTLS(t, srv)
	if _, err := GatewayModels(context.Background(), liveDbxProvider(t, srv.URL, "tok")); err == nil {
		t.Error("404 listing returned no error")
	}
}
