package recall

import (
	"os"
	"strings"
	"sync"
	"testing"
)

// A corrupt or relative root-<id> file is replaced by the first valid root instead of lingering all session.
func TestRecordSessionRootRepairsCorruptFile(t *testing.T) {
	isolatedConfig(t)
	const id = "11111111-2222-3333-4444-555555555555"
	cwd := canonTemp(t, t.TempDir())
	for _, bad := range []string{"relative/path\n", "\n"} {
		path := sessionRootPath(id)
		write(t, path, bad)
		RecordSessionRoot(id, cwd)
		got, err := os.ReadFile(path)
		if err != nil || !strings.HasPrefix(string(got), "/") {
			t.Fatalf("bad %q not repaired: %q, %v", bad, got, err)
		}
		if SessionRoot(id, "fallback") == "fallback" {
			t.Errorf("SessionRoot still falls back after repairing %q", bad)
		}
	}
}

// A valid existing root is kept: the first root wins.
func TestRecordSessionRootKeepsValidFile(t *testing.T) {
	isolatedConfig(t)
	const id = "11111111-2222-3333-4444-555555555555"
	keep := canonTemp(t, t.TempDir())
	write(t, sessionRootPath(id), keep+"\n")
	RecordSessionRoot(id, canonTemp(t, t.TempDir()))
	if got := SessionRoot(id, ""); got != keep {
		t.Errorf("first root replaced: got %q, want %q", got, keep)
	}
}

// Racing repairs of one corrupt root leave one root, and every caller sees that same root once it returns.
func TestRecordSessionRootRacingRepairsAgree(t *testing.T) {
	isolatedConfig(t)
	const id = "11111111-2222-3333-4444-555555555555"
	cwds := make([]string, 6)
	for i := range cwds {
		cwds[i] = canonTemp(t, t.TempDir())
	}
	for round := range 40 {
		write(t, sessionRootPath(id), "relative/path\n")
		seen := make([]string, len(cwds))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, cwd := range cwds {
			wg.Go(func() {
				<-start
				RecordSessionRoot(id, cwd)
				seen[i] = SessionRoot(id, "")
			})
		}
		close(start)
		wg.Wait()
		final := SessionRoot(id, "")
		for i, got := range seen {
			if got == "" || got != final {
				t.Fatalf("round %d: caller %d saw %q, final root %q", round, i, got, final)
			}
		}
	}
}
