package server

import (
	"bytes"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func replyOf(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

// Only the explicit deprecated 400 on a Databricks row lands in the drop list, once, with a log line; undrop clears it.
func TestNoteDeprecatedDropsOnlyTheExplicitAnswer(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"dev","providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.example.com","auth":{"type":"none"}}}}}`)
	r := newRouter("")
	reg, err := r.registry()
	if err != nil || reg.Providers["dev"] == nil {
		t.Fatalf("registry = %v, %v", reg, err)
	}
	dbx := reg.Providers["dev"]
	var log bytes.Buffer
	r.log = &log
	const dep = `{"error_code":"BAD_REQUEST","message":"This endpoint databricks-gpt-5-mini is deprecated."}`
	for _, c := range []*http.Response{replyOf(400, `{"message":"bad request shape"}`), replyOf(429, dep),
		replyOf(503, dep), replyOf(200, dep), replyOf(400, `{"message":"endpoint databricks-x is deprecated soon"}`)} {
		r.noteUnserved(dbx, c)
	}
	other := *dbx
	other.Preset = ""
	r.noteUnserved(&other, replyOf(400, dep))
	r.bg.Wait()
	if got, _ := catalog.PickerDropped(dbx); len(got) != 0 {
		t.Fatalf("dropped %v after non-qualifying replies, want none", got)
	}
	resp := replyOf(400, dep)
	r.noteUnserved(dbx, resp)
	r.noteUnserved(dbx, replyOf(400, dep))
	r.bg.Wait()
	got, _ := catalog.PickerDropped(dbx)
	if !slices.Equal(got, []string{"databricks-gpt-5-mini"}) || strings.Count(log.String(), "is deprecated; dropped") != 1 {
		t.Fatalf("dropped %v, log %q; want the id once and one log line", got, log.String())
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != dep {
		t.Errorf("body after peek = %q, want it intact", body)
	}
	if why, _ := catalog.DroppedReasons(dbx); !strings.HasPrefix(why["databricks-gpt-5-mini"], "deprecated 20") {
		t.Errorf("reasons = %v, want a dated deprecated reason", why)
	}
	undropClears(t, dbx)
}

func undropClears(t *testing.T, dbx *providers.Provider) {
	t.Helper()
	if _, err := catalog.EditPickerDrops(dbx, []string{"databricks-gpt-5-mini"}, true, true); err != nil {
		t.Fatal(err)
	}
	got, _ := catalog.PickerDropped(dbx)
	why, _ := catalog.DroppedReasons(dbx)
	if len(got) != 0 || len(why) != 0 {
		t.Errorf("after undrop dropped %v reasons %v, want none", got, why)
	}
}

// A deprecated 400 seen on a routed copy of the row is recorded in the registry row's scope, where the CLI readers look.
func TestFailoverRecordsInTheScopeTheCLIReads(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"dev","providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.example.com","auth":{"type":"none"},"models":{"opus":"databricks-gpt-5-mini"}}}}}`)
	r := newRouter(secret)
	r.log = io.Discard
	reg, err := r.registry()
	if err != nil || reg.Providers["dev"] == nil {
		t.Fatalf("registry = %v, %v", reg, err)
	}
	routed := *reg.Providers["dev"]
	routed.BaseURL, routed.Headers = "https://other.example.com/d/abc/", map[string]string{"X-Tag": "t"}
	const dep = `{"message":"This endpoint databricks-gpt-5-mini is deprecated."}`
	r.failover(nil, replyOf(400, dep), providers.Target{Provider: &routed, Model: "databricks-gpt-5-mini"}, true)
	r.bg.Wait()
	fresh, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	dropped, err := catalog.DroppedIDs(fresh.Providers["dev"])
	if err != nil || !dropped[catalog.ID("databricks-gpt-5-mini")] {
		t.Fatalf("CLI-side dropped = %v, %v; want the id", dropped, err)
	}
	if routedDropped, _ := catalog.PickerDropped(&routed); len(routedDropped) != 0 {
		t.Errorf("routed copy's scope holds %v, want the record only in the registry row's scope", routedDropped)
	}
}
