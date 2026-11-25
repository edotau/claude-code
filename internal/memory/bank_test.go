package memory

import (
	"fmt"
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

func TestRequireFlatNewerRefusesOlderNotes(t *testing.T) {
	isolatedConfig(t)
	stamp := filepath.Join(paths.MemoryStoreDir(), layoutFile)
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		stamp   string
		wantErr string
	}{{"flat 9", "store is flat 9, this binary writes flat 4: install a newer claude-code"}, {"flat 3", ""}, {"flat 4", ""}} {
		if err := os.WriteFile(stamp, []byte(tc.stamp+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		forgetLayout()
		err := requireFlat()
		if got := fmt.Sprint(err); (tc.wantErr == "" && err != nil) || (tc.wantErr != "" && got != tc.wantErr) {
			t.Errorf("requireFlat on %q = %v, want %q", tc.stamp, err, tc.wantErr)
		}
		if b, _ := os.ReadFile(stamp); string(b) != tc.stamp+"\n" {
			t.Errorf("stamp = %q, want %q untouched", b, tc.stamp)
		}
	}
	if store, code, err := StoreVersions(); err != nil || store != 4 || code != storeVersion {
		t.Errorf("StoreVersions = %d, %d, %v", store, code, err)
	}
}
