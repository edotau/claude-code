package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const deadPID = 2147483646

// regFixture lays down a registry entry plus its transcript (aged by age) under claudeDir.
func regFixture(t *testing.T, claudeDir, id string, pid int, age time.Duration, now time.Time) {
	t.Helper()
	entry, _ := json.Marshal(regEntry{PID: pid, SessionID: id, Cwd: "/repo", Name: "name-" + id, Status: "waiting", Kind: "interactive"})
	reg := filepath.Join(claudeDir, "sessions", fmt.Sprintf("%d.json", pid))
	tr := filepath.Join(claudeDir, "projects", "-repo", id+".jsonl")
	for p, body := range map[string]string{
		reg: string(entry),
		tr:  `{"type":"user","cwd":"/repo","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mt := now.Add(-age)
	if err := os.Chtimes(tr, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// TestRegistryJoin: live pid beats a stale transcript under --running; a dead pid and an unparsable file add nothing.
func TestRegistryJoin(t *testing.T) {
	claudeDir := t.TempDir()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	old := pidAlive
	pidAlive = func(pid int) bool { return pid == os.Getpid() }
	t.Cleanup(func() { pidAlive = old })

	regFixture(t, claudeDir, "live-stale", os.Getpid(), 40*24*time.Hour, now)
	regFixture(t, claudeDir, "dead-stale", deadPID, 40*24*time.Hour, now)
	regFixture(t, claudeDir, "dead-fresh", deadPID-1, time.Minute, now)
	for name, body := range map[string]string{"junk.json": "{not json", "noid.json": `{"pid":1,"name":"x"}`} {
		if err := os.WriteFile(filepath.Join(claudeDir, "sessions", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	tree, err := Build(claudeDir, Options{Days: 1, Running: true, Now: pinNow(now)})
	if err != nil {
		t.Fatalf("Build with an unparsable registry file: %v", err)
	}
	var got *Session
	for i, s := range tree.Sessions {
		switch s.ID {
		case "dead-stale":
			t.Errorf("dead pid with stale transcript listed under --running")
		case "dead-fresh":
			if s.Live || s.PID != 0 {
				t.Errorf("dead pid marked live: %+v", s)
			}
		case "live-stale":
			got = &tree.Sessions[i]
		}
	}
	if got == nil {
		t.Fatal("live pid with a 40-day-old transcript dropped by --running")
	}
	if !got.Live || got.PID != os.Getpid() || got.Name != "name-live-stale" || got.CLIStatus != "waiting" || got.Mode != "interactive" {
		t.Errorf("registry fields not joined: %+v", got)
	}
}

// TestReadRegistrySkipsUnusableEntries: a live pid without a sessionId and an unparsable file yield no entry.
func TestReadRegistrySkipsUnusableEntries(t *testing.T) {
	claudeDir := t.TempDir()
	old := pidAlive
	pidAlive = func(int) bool { return true }
	t.Cleanup(func() { pidAlive = old })
	files := map[string]string{"junk.json": "{not json", "noid.json": `{"pid":7,"name":"x"}`, "ok.json": `{"pid":8,"sessionId":"s8"}`}
	for name, body := range files {
		p := filepath.Join(claudeDir, "sessions", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := readRegistry(claudeDir); len(got) != 1 || got["s8"].PID != 8 {
		t.Errorf("readRegistry = %+v, want only s8", got)
	}
}
