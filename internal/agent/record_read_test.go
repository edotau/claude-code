package agent

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/routing/inflight"
	"github.com/edotau/claude-code/internal/routing/route"
)

// RecordsSince drops only files last written before since and keeps the rest newest first.
func TestRecordsSince(t *testing.T) {
	setConfigDir(t)
	now := time.Now()
	var ids []string
	for i := range 3 {
		ids = append(ids, fixtureRecord(t, NewRecord("codex", RunOptions{Prompt: "p"}, now.Add(time.Duration(i-3)*time.Hour))).ID)
	}
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(paths.DispatchDir(), ids[0]+".json"), old, old); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range RecordsSince(now.Add(-time.Hour)) {
		got = append(got, r.ID)
	}
	if want := []string{ids[2], ids[1]}; !slices.Equal(got, want) {
		t.Errorf("RecordsSince = %v, want %v", got, want)
	}
}

// ReadRecentWith selects what a full decode plus the caller's own check selects, decoys and torn files included.
func TestReadRecentWithMatchesFullDecode(t *testing.T) {
	setConfigDir(t)
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	parent := "20261016T210000.000-codex"
	save := func(i int, mut func(*Record)) {
		r := NewRecord("codex", RunOptions{Prompt: "p"}, base.Add(time.Duration(i)*time.Minute))
		mut(&r)
		fixtureRecord(t, r)
	}
	done := wire.Result{Model: "m"}
	save(0, func(r *Record) { r.Parent = "" })
	save(1, func(r *Record) { r.Parent = parent })
	save(2, func(r *Record) { r.Parent = parent; r.Finish(done, nil, time.Second) })
	save(3, func(r *Record) { r.Finish(wire.Result{}, errors.New("failed"), time.Second) })
	save(4, func(r *Record) {
		r.Request.Prompt = `say "status": "running" and "` + parent + `"`
		r.Finish(done, nil, time.Second)
	})
	save(5, func(r *Record) { r.Error = "running " + parent; r.Finish(done, nil, time.Second); r.Parent = "other" })
	save(6, func(r *Record) { r.Request.Prompt = "\"running\" " + parent; r.Finish(done, nil, time.Second) })
	writeRawDispatch(t, "20261016T220000.000-torn", `{"schema":1,"status":"running","parent":"`+parent)
	writeRawDispatch(t, "20261016T220001.000-empty", "")
	for name, tc := range map[string]struct {
		needle string
		keep   func(Record) bool
	}{
		"running": {inflight.RunningToken, func(r Record) bool { return r.Status == "running" }},
		"parent":  {jsonToken(parent), func(r Record) bool { return r.Parent == parent }},
		"absent":  {jsonToken("nobody"), func(r Record) bool { return r.Parent == "nobody" }},
	} {
		got, err := ReadRecentWith(AllRecords, tc.needle)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range got {
			if tc.keep(r) {
				ids = append(ids, r.ID)
			}
		}
		if want := prefilterOracle(tc.keep); !slices.Equal(ids, want) || len(want) == 0 && name != "absent" {
			t.Errorf("%s: prefiltered = %v, full decode = %v", name, ids, want)
		}
	}
}

// A FIFO named like a record (newest) is skipped by both read paths instead of blocking them.
func TestReadRecentSkipsFIFO(t *testing.T) {
	mkfifo := "/usr/bin/mkfifo" // TestMain's PATH holds no system tools
	if _, err := os.Stat(mkfifo); err != nil {
		t.Skip("no mkfifo")
	}
	setConfigDir(t)
	want := fixtureRecord(t, NewRecord("codex", RunOptions{Prompt: "p"}, time.Now().Add(-time.Minute))).ID
	if out, err := exec.Command(mkfifo, filepath.Join(paths.DispatchDir(), "99991231T235959.999-hang.json")).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v %s", err, out)
	}
	done := make(chan []Record, 2)
	go func() {
		one, _ := ReadRecent(1)
		all, _ := ReadRecentWith(AllRecords, "")
		done <- one
		done <- all
	}()
	for _, path := range []string{"ReadRecent", "readAllRecent"} {
		select {
		case got := <-done:
			if len(got) != 1 || got[0].ID != want {
				t.Errorf("%s = %v, want only %s", path, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s blocked on the FIFO", path)
		}
	}
}

// BenchmarkRunningRecentAllRecords2000 scans a full dispatch log (the 2000 kept records), 5 of them running.
func BenchmarkRunningRecentAllRecords2000(b *testing.B) {
	setConfigDir(b)
	dir := paths.DispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.Fatal(err)
	}
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	running := map[int]bool{10: true, 1000: true, 1500: true, 1990: true, 1999: true}
	for i := range DispatchKeep() {
		rec := benchAskRecord(base.Add(time.Duration(i)*time.Minute), i)
		if !running[i] {
			rec.Status, rec.DurationMs, rec.Usage = "ok", 93_000, &wire.Usage{InputTokens: 41_000, OutputTokens: 2_300}
		}
		body, err := rec.encode()
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rec.ID+".json"), body, 0o600); err != nil {
			b.Fatal(err)
		}
	}
	if got, err := RunningRecent(AllRecords); err != nil || len(got) != len(running) {
		b.Fatalf("RunningRecent = %d records, %v; want %d", len(got), err, len(running))
	}
	b.ReportAllocs()
	for b.Loop() {
		_, _ = RunningRecent(AllRecords)
	}
}

// An older record's provider, ran_identity and fallback keys still read; a new one stores them in ran_route.
func TestDecodeOldRecordKeys(t *testing.T) {
	old := `{"id":"x","agent":"codex","provider":"dev","ran_identity":"sp","account":"acct","fallback":"claude","fallback_policy":"all","ran_route":"databricks:dev"}`
	rec, err := decodeRecord([]byte(old))
	if err != nil || rec.Provider != "dev" || rec.RanIdentity != "sp" || rec.Fallback != "claude" || rec.Account != "acct" || rec.FallbackPolicy != "all" {
		t.Fatalf("old record = %+v err=%v", rec, err)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, `"provider":"dev"`) || strings.Contains(s, "ran_identity") || !strings.Contains(s, `"billing_key":"acct"`) || !strings.Contains(s, `"identity":"sp"`) {
		t.Errorf("encoded = %s, want identity inside ran_route and no provider key", s)
	}
	again, err := decodeRecord(b)
	if err != nil || again.RanRoute != (route.Route{Provider: "databricks", Profile: "dev", Identity: "sp"}) || again.Provider != "dev" || again.RanIdentity != "sp" {
		t.Errorf("round trip = %+v err=%v", again, err)
	}
}

// A provider that differs from ran_route.profile loses to the route; a legacy provider fills a route with no profile.
func TestRecordRouteProfileBeatsLegacyProvider(t *testing.T) {
	got, err := decodeRecord([]byte(`{"id":"x","provider":"qa","ran_route":{"provider":"databricks","profile":"dev"}}`))
	if err != nil || got.Provider != "dev" {
		t.Errorf("got %+v err=%v, want provider dev", got, err)
	}
	got, err = decodeRecord([]byte(`{"id":"x","provider":"qa","ran_route":{"provider":"databricks"}}`))
	if err != nil || got.Provider != "qa" {
		t.Errorf("got %+v err=%v, want legacy provider qa", got, err)
	}
}

// A present canonical key beats the older alias; only a Databricks answer has an identity; a bare provider is a profile
func TestRecordAliasesAndIdentityRules(t *testing.T) {
	got, err := decodeRecord([]byte(`{"id":"x","billing_key":"","account":"old","fallback_from":"","fallback":"codex"}`))
	if err != nil || got.Account != "" || got.Fallback != "" {
		t.Errorf("got %+v err=%v, want the present empty canonical keys kept", got, err)
	}
	b, _ := json.Marshal(Record{ID: "x", Provider: "dev", RanIdentity: "sp", RanRoute: route.Route{Provider: "openai", Profile: "p"}})
	if s := string(b); strings.Contains(s, `"identity"`) {
		t.Errorf("encoded = %s, want no identity on an openai row", s)
	}
	b, _ = json.Marshal(Record{ID: "x", Provider: "dev"})
	if s := string(b); !strings.Contains(s, `"ran_route":{"profile":"dev"}`) {
		t.Errorf("encoded = %s, want ran_route holding the profile only", s)
	}
}

func TestRunningRecentActiveIndexLifecycleAndFallback(t *testing.T) {
	setConfigDir(t)
	dir := paths.DispatchDir()
	base := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	r1 := saveRunning(t, "codex", "one", base)
	r2 := saveRunning(t, "claude", "two", base.Add(time.Minute))
	wantRunning(t, []string{r2.ID, r1.ID})
	r1.Status = wire.StatusOK
	saveRecordForTest(t, r1)
	wantNoMarker(t, dir, r1.ID)
	// Stale marker (record finished on disk without Save) is lazily unlinked by RunningRecent.
	if err := os.WriteFile(markerPath(dir, r1.ID), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wantRunning(t, []string{r2.ID})
	wantNoMarker(t, dir, r1.ID)
}

func TestRunningRecentActiveFallbackRawFixtures(t *testing.T) {
	setConfigDir(t)
	dir := paths.DispatchDir()
	okAt := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	liveAt := okAt.Add(time.Minute)
	writeEncodedRaw(t, NewRecord("codex", RunOptions{Prompt: "done"}, okAt), wire.StatusOK)
	live := NewRecord("claude", RunOptions{Prompt: "go"}, liveAt)
	writeEncodedRaw(t, live, "running")
	wantRunning(t, []string{live.ID})
	if _, err := os.Stat(markerPath(dir, live.ID)); err != nil {
		t.Fatalf("fallback did not seed active marker: %v", err)
	}
}

func TestReconcileActiveRepairsDrift(t *testing.T) {
	setConfigDir(t)
	dir := paths.DispatchDir()
	live := saveRunning(t, "codex", "live", time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC))
	_ = os.Remove(markerPath(dir, live.ID))
	ghost := markerPath(dir, "20261127T090000.000-ghost")
	if err := os.WriteFile(ghost, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ReconcileActive(dir)
	if _, err := os.Stat(markerPath(dir, live.ID)); err != nil {
		t.Fatalf("ReconcileActive did not restore missing marker: %v", err)
	}
	wantNoMarker(t, dir, "20261127T090000.000-ghost")
}

// TestReconcileActiveKeepsLiveClaimedDuringRebuild keeps a marker claimed between snapshot and delete.
func TestReconcileActiveKeepsLiveClaimedDuringRebuild(t *testing.T) {
	setConfigDir(t)
	dir := paths.DispatchDir()
	_ = saveRunning(t, "codex", "old", time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC))
	id := recordID(time.Date(2026, 11, 27, 12, 1, 0, 0, time.UTC), "claude")
	t.Cleanup(func() { activeRebuildHook = nil })
	activeRebuildHook = func(d string) { claimLiveDuringRebuild(t, d, id) }
	seedActiveIndex(dir, nil)
	if _, err := os.Stat(markerPath(dir, id)); err != nil {
		t.Fatalf("rebuild deleted the marker claimed after its snapshot: %v", err)
	}
	if _, err := os.Stat(indexedPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("torn rebuild certified .indexed: %v", err)
	}
}

// TestSaveDoesNotCertifyStaleIndex drops .indexed after a failed marker write and after a raw extra record.
func TestSaveDoesNotCertifyStaleIndex(t *testing.T) {
	setConfigDir(t)
	dir := paths.DispatchDir()
	r := saveRunning(t, "codex", "one", time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC))
	if _, err := os.Stat(indexedPath(dir)); err != nil {
		t.Fatalf("first Save did not stamp .indexed: %v", err)
	}
	blockMarker(t, dir, r.ID)
	r.Request.Prompt = "changed"
	saveRecordForTest(t, r)
	if _, err := os.Stat(indexedPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("failed marker write left .indexed: %v", err)
	}
	if err := os.Remove(markerPath(dir, r.ID)); err != nil {
		t.Fatal(err)
	}
	wantRawSaveDropsIndex(t, dir, r)
}

func BenchmarkRunningRecentActiveIndex(b *testing.B) {
	setConfigDir(b)
	dir := paths.DispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.Fatal(err)
	}
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	running := map[int]bool{10: true, 1000: true, 1500: true, 1990: true, 1999: true}
	for i := range DispatchKeep() {
		rec := benchAskRecord(base.Add(time.Duration(i)*time.Minute), i)
		if !running[i] {
			rec.Status, rec.DurationMs, rec.Usage = "ok", 93_000, &wire.Usage{InputTokens: 41_000, OutputTokens: 2_300}
		}
		body, err := rec.encode()
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rec.ID+".json"), body, 0o600); err != nil {
			b.Fatal(err)
		}
		syncActiveMarker(dir, rec.ID, running[i])
	}
	if got, err := RunningRecent(AllRecords); err != nil || len(got) != len(running) {
		b.Fatalf("RunningRecent = %d records, %v; want %d", len(got), err, len(running))
	}
	b.ReportAllocs()
	for b.Loop() {
		_, _ = RunningRecent(AllRecords)
	}
}

// writeEncodedRaw writes a record the way Save does, without touching the active index.
func writeEncodedRaw(t *testing.T, r Record, status string) {
	t.Helper()
	r.Status = status
	body, err := r.encode()
	if err != nil {
		t.Fatal(err)
	}
	writeRawDispatch(t, r.ID, string(body))
}

func saveRunning(t *testing.T, agentName, prompt string, at time.Time) Record {
	t.Helper()
	r := NewRecord(agentName, RunOptions{Prompt: prompt}, at)
	saveRecordForTest(t, r)
	return r
}

func wantRunning(t *testing.T, ids []string) {
	t.Helper()
	got, err := RunningRecent(AllRecords)
	if err != nil || len(got) != len(ids) {
		t.Fatalf("RunningRecent = %+v, %v; want %v", got, err, ids)
	}
	for i, id := range ids {
		if got[i].ID != id {
			t.Fatalf("RunningRecent[%d] = %s, want %s", i, got[i].ID, id)
		}
	}
}

func markerPath(dir, id string) string { return filepath.Join(activeDir(dir), id+".json") }

// blockMarker replaces id's marker file with a directory so the next WriteFile of it fails.
func blockMarker(t *testing.T, dir, id string) {
	t.Helper()
	path := markerPath(dir, id)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func wantNoMarker(t *testing.T, dir, id string) {
	t.Helper()
	if _, err := os.Stat(markerPath(dir, id)); !os.IsNotExist(err) {
		t.Fatalf("marker %s still present: %v", id, err)
	}
}

// claimLiveDuringRebuild writes a running record and its marker after the rebuild snapshotted dir, once.
func claimLiveDuringRebuild(t *testing.T, dir, id string) {
	t.Helper()
	activeRebuildHook = nil
	body := []byte(`{"schema":1,"id":"` + id + `","agent":"claude","status":"running"}`)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath(dir, id), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	noteActiveGen() // a real Save/claim bumps the generation the locked snapshot compares
	now := time.Now()
	if err := os.Chtimes(dir, now, now); err != nil {
		t.Fatal(err)
	}
}

// wantRawSaveDropsIndex adds an unindexed running record, Saves another, and requires the index to be dropped.
func wantRawSaveDropsIndex(t *testing.T, dir string, r Record) {
	t.Helper()
	ReconcileActive(dir)
	if _, err := os.Stat(indexedPath(dir)); err != nil {
		t.Fatalf("reconcile did not stamp .indexed: %v", err)
	}
	rawAt := time.Date(2026, 11, 27, 13, 1, 0, 0, time.UTC)
	raw := NewRecord("claude", RunOptions{Prompt: "raw"}, rawAt)
	writeEncodedRaw(t, raw, "running")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(dir, future, future); err != nil {
		t.Fatal(err)
	}
	r.Request.Prompt = "again"
	saveRecordForTest(t, r)
	if _, err := os.Stat(indexedPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("Save certified a stale index over raw fixture: %v", err)
	}
	got, err := RunningRecent(AllRecords)
	if err != nil || len(got) != 2 {
		t.Fatalf("RunningRecent after raw fixture = %+v, %v; want both running records", got, err)
	}
}
