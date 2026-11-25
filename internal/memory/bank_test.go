package memory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

func TestBankPaths(t *testing.T) {
	isolatedConfig(t)
	s := paths.MemoryStoreDir()
	for got, want := range map[string]string{
		Path("activeContext.md"):               "activeContext.md",
		paths.MemorySub("reports", "x.md"):     "reports/x.md",
		paths.MemorySub("techdebt", "area.md"): "techdebt/area.md",
		NativeIndex():                          "MEMORY.md",
	} {
		if got != filepath.Join(s, want) {
			t.Errorf("got %s, want %s", got, filepath.Join(s, want))
		}
	}
	if got := paths.MemorySubDir("reports"); got != filepath.Join(s, "reports") {
		t.Errorf("paths.MemorySubDir(reports) = %s", got)
	}
}

func TestStoreFlatReadsStamp(t *testing.T) {
	isolatedConfig(t)
	if storeFlat() {
		t.Fatal("unstamped store reads flat")
	}
	write(t, filepath.Join(paths.MemoryStoreDir(), layoutFile), layoutFlat+"\n")
	forgetLayout()
	if !storeFlat() {
		t.Fatal("stamped store reads legacy")
	}
}

func TestInitStampsFreshStore(t *testing.T) {
	isolatedConfig(t)
	if _, err := Init(); err != nil || !storeFlat() {
		t.Errorf("Init on a fresh store = %v, flat=%v, want it stamped", err, storeFlat())
	}
}

func TestRequireFlatRefusesAnOlderStamp(t *testing.T) {
	isolatedConfig(t)
	stamp := filepath.Join(paths.MemoryStoreDir(), layoutFile)
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stamp, []byte("flat 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	forgetLayout()
	if err := requireFlat(); err == nil {
		t.Fatal("requireFlat on a flat 2 store = nil, want a refusal")
	}
	if b, _ := os.ReadFile(stamp); string(b) != "flat 2\n" {
		t.Errorf("stamp = %q, want it untouched", b)
	}
}
