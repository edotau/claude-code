package tree

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildRejectsSessionGlobPatterns(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	for _, id := range []string{"*", "?", "[", "[rs]*", `running\-session`} {
		t.Run(id, func(t *testing.T) {
			got, err := Build(claudeDir, Options{Session: id, Days: 1, Now: pinNow(now)})
			if err == nil {
				t.Fatalf("Build(Session=%q) = %+v, want rejection instead of glob expansion", id, got)
			}
			if !strings.Contains(err.Error(), "not a plain session id") {
				t.Errorf("Build(Session=%q) error = %v, want plain-session validation", id, err)
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
	for _, running := range []bool{true, false} {
		got, err := Listing(claudeDir, Options{Days: 1, Running: running, Now: pinNow(now)}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Sessions) != 1 {
			t.Fatalf("Listing(Running=%t) sessions = %+v, want only the old live session", running, got.Sessions)
		}
		s := got.Sessions[0]
		if s.ID != "live-old" || !s.Live || s.PID != os.Getpid() {
			t.Errorf("Listing(Running=%t) session = %+v, want the live registered session", running, s)
		}
	}
}
