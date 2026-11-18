package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestLearnRecall(t *testing.T) {
	tempConfig(t)
	now := time.Now().UTC()
	const elsewhere = "22222222-2222-2222-2222-222222222222"
	write(t, Path("conventions.md"), `# Conventions

- Used <!-- id:aaaa0001 -->
- Unused <!-- id:aaaa0002 -->
`)
	dir := accessDir()
	sessions := make([]string, recallMinInjections)
	for i := range sessions {
		sessions[i] = fmt.Sprintf("11111111-1111-1111-1111-%012d", i)
		logAccessBatch(dir, now.Add(-time.Minute), []string{"aaaa0001", "aaaa0002"}, accessRecall, sessions[i])
	}
	logAccessBatch(dir, now.Add(-time.Minute), []string{"aaaa0003"}, accessRecall, sessions[0])
	for _, s := range sessions {
		t.Setenv(SessionIDEnv, s)
		showRecallFixture(t, "aaaa0001")
	}
	t.Setenv(SessionIDEnv, elsewhere)
	showRecallFixture(t, "aaaa0002")
	if err := LearnRecall(now); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]float64{"aaaa0001": 1.2, "aaaa0002": 0.8, "aaaa0003": 1} {
		if got := boostFactor(recallWeights(), id); got != want {
			t.Errorf("%s factor = %v, want %v", id, got, want)
		}
	}
	assertRecallNoRewrite(t, now)
}

func showRecallFixture(t *testing.T, id string) {
	t.Helper()
	if _, _, err := Show(id); err != nil {
		t.Fatal(err)
	}
}

func assertRecallNoRewrite(t *testing.T, now time.Time) {
	t.Helper()
	path := recallLearningPath()
	stamp := now.Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := LearnRecall(now); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(stamp) {
		t.Fatal("unchanged access log caused the learner to rewrite state")
	}
}

func BenchmarkRecallRanking(b *testing.B) {
	b.Cleanup(paths.SetStampNowForTest(func() time.Time { return time.Now().Add(time.Hour) }))
	for _, present := range []bool{false, true} {
		b.Run(fmt.Sprintf("weights=%t", present), func(b *testing.B) {
			b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
			b.Setenv("HOME", b.TempDir())
			b.Setenv(UpdateChildEnv, "")
			var bank strings.Builder
			bank.WriteString("# Conventions\n\n")
			for i := range 5 {
				fmt.Fprintf(&bank, "- deploy pipeline staging signoff rule %d <!-- id:%08x -->\n", i, i+1)
			}
			write(b, Path("conventions.md"), bank.String())
			logAccess(accessDir(), time.Now(), "00000001", AccessShow)
			if present {
				path := filepath.Join(StateDir(), "recall-learning.json")
				write(b, path, `{"days":{"2026-11-18":{"00000001":{"injected":5,"used":5}}}}`)
			}
			recallPicks(context.Background(), "", recallTask, false, false)
			for b.Loop() {
				recallPicks(context.Background(), "", recallTask, false, false)
			}
		})
	}
}
