package tree

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildRejectsSessionGlobPatterns(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	for _, id := range []string{"*", "?", "[", "[rs]*", `running\-session`, "../../etc/passwd", "foo/../bar", ".."} {
		t.Run(id, func(t *testing.T) {
			got, err := build(claudeDir, Options{Session: id, Days: 1, Now: pinNow(now)})
			if err == nil {
				t.Fatalf("build(Session=%q) = %+v, want rejection instead of glob expansion", id, got)
			}
			if !strings.Contains(err.Error(), "not a plain session id") {
				t.Errorf("build(Session=%q) error = %v, want plain-session validation", id, err)
			}
		})
	}
}

func TestBackgroundKeepsLiveSessionWithOldTranscript(t *testing.T) {
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	regFixture(t, claudeDir, "live-old", os.Getpid(), 40*24*time.Hour, now)
	regFixture(t, claudeDir, "dead-old", deadPID, 40*24*time.Hour, now)
	got, err := listing(claudeDir, Options{Days: 1, Now: pinNow(now)}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("listing sessions = %+v, want only the old live session", got.Sessions)
	}
	if s := got.Sessions[0]; s.ID != "live-old" || !s.Live || s.PID != os.Getpid() {
		t.Errorf("listing session = %+v, want the live registered session", s)
	}
}
