package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
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
