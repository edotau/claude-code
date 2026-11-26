package doctor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestLocalRow(t *testing.T) {
	at := time.Date(2026, 11, 25, 14, 2, 0, 0, time.Local)
	cases := []struct {
		name, kind, surface, stage, want string
		st                               router.LocalState
		at                               time.Time
		ok                               bool
	}{
		{name: "up with a fallback", st: router.LocalState{Up: true, Model: "qwen3.8-27b:gguf:q6", Running: 1},
			at: at, kind: "rate limited", surface: "dispatch", stage: "last", ok: true,
			want: "up · qwen3.8-27b:gguf:q6 · 1 running · last fallback 2026-11-25 14:02 rate limited (dispatch) · " +
				"stage last"},
		{name: "down hides the fallback", st: router.LocalState{Reason: "connection refused"}, at: at, stage: "shadow",
			want: "down (connection refused) · stage shadow"},
		{name: "no row", st: router.LocalState{Reason: string(router.LocalNoRow)},
			want: "down (no last-resort row) · stage unset"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := localCheck(c.st, c.at, c.kind, c.surface, c.stage)
			if got.Name != "local" || got.OK != c.ok || got.Hard || got.Detail != c.want {
				t.Fatalf("got %+v, want ok=%v %q", got, c.ok, c.want)
			}
		})
	}
}

// localRow probes the registry's last-resort row: up when the server lists the row's model.
func TestLocalRowProbes(t *testing.T) {
	vaulttest.Use(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3.8-27b:gguf:q6"}]}`))
	}))
	t.Cleanup(srv.Close)
	vaulttest.Put(t, vault.Overlay, "providers",
		`{"providers":{"magnitude":{"local":{"protocol":"openai","base_url":"`+srv.URL+`/inference/v1",`+
			`"routes":{"anthropic":"`+srv.URL+`/inference/anthropic"},"concurrency":1,`+
			`"options":{"last_resort":true},"auth":{"type":"key","command":"echo local"},`+
			`"models":{"sonnet":"qwen3.8-27b:gguf:q6"}}}}}`)
	got := localRow()
	want := "up · qwen3.8-27b:gguf:q6 · 0 running"
	if got.Name != "local" || !got.OK || got.Hard || !strings.HasPrefix(got.Detail, want) {
		t.Fatalf("localRow = %+v, want up on the overlaid row", got)
	}
}
