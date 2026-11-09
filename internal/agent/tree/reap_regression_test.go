package tree

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/inflight"
)

type reapWriteFunc func([]byte) (int, error)

func (f reapWriteFunc) Write(p []byte) (int, error) { return f(p) }

func reapSocketConfig(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
}

func saveReapRecord(t *testing.T, pid int, at time.Time) job.Record {
	t.Helper()
	rec := job.ClaimRecord("claude", agent.RunOptions{}, at)
	rec.Live, rec.PID = true, pid
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestReapKeepsSocketCreatedAfterRecordSnapshot(t *testing.T) {
	reapSocketConfig(t)
	now := time.Now()
	saveReapRecord(t, deadPID, now.Add(-time.Minute))
	var socket string
	out := reapWriteFunc(func(p []byte) (int, error) {
		if strings.HasPrefix(string(p), "reaped dispatch ") {
			rec := saveReapRecord(t, os.Getpid(), now)
			socket = job.LiveSocket(rec.ID)
			ln, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := ln.Close(); err != nil {
					t.Error(err)
				}
			})
		}
		return len(p), nil
	})
	var errw bytes.Buffer
	if code := Reap(false, out, &errw); code != 0 {
		t.Fatalf("Reap = %d: %s", code, &errw)
	}
	if socket == "" {
		t.Fatal("dispatch output did not trigger the post-snapshot session start")
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("new live session's socket was removed after the record snapshot: %v", err)
	}
}

func TestReapReportsRemovalFailures(t *testing.T) {
	for _, kind := range []string{"socket", "marker"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			path := job.LiveSocket("orphan")
			if kind == "marker" {
				path = filepath.Join(inflight.Dir(), "agent-2147483646-stale")
			}
			// A nonempty entry makes os.Remove fail even when the tests run as root.
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "child"), []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errw bytes.Buffer
			if code := Reap(false, &out, &errw); code != 1 {
				t.Fatalf("Reap = %d, want 1 for failed %s removal; stderr=%s", code, kind, &errw)
			}
			if !strings.Contains(errw.String(), kind+" "+filepath.Base(path)) {
				t.Errorf("stderr = %q, want failed %s name", errw.String(), kind)
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("failed removal must leave the entry in place: %v", err)
			}
		})
	}
}
