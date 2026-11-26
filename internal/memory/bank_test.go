package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
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

func TestRequireFlatNewerRefusesOlderNotes(t *testing.T) {
	isolatedConfig(t)
	stamp := filepath.Join(paths.MemoryStoreDir(), layoutFile)
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ stamp, wantErr string }{
		{"flat 9", "store is flat 9, this binary writes flat 4: install a newer claude-code"}, {"flat 4", ""},
	} {
		if err := os.WriteFile(stamp, []byte(tc.stamp+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := stampLayout(); (tc.wantErr == "" && err != nil) || (tc.wantErr != "" && fmt.Sprint(err) != tc.wantErr) {
			t.Errorf("stampLayout on %q = %v, want %q", tc.stamp, err, tc.wantErr)
		}
		if b, _ := os.ReadFile(stamp); string(b) != tc.stamp+"\n" {
			t.Errorf("stamp = %q, want %q untouched", b, tc.stamp)
		}
	}
}

func TestStampLayoutUpgradesOlderStamp(t *testing.T) {
	for v := 1; v < storeVersion; v++ {
		t.Run(strconv.Itoa(v), func(t *testing.T) {
			isolatedConfig(t)
			write(t, Path(layoutFile), fmt.Sprintf("flat %d\n", v))
			const body = "# Notes\n\n## repeated\nbody\n\n## repeated\nundated and id-less\n"
			write(t, Path("decisionLog.md"), body)
			if err := EnsureBank(""); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(Path(layoutFile)); err != nil || string(b) != layoutFlat+"\n" {
				t.Fatalf("stamp = %q, err=%v", b, err)
			}
			if b, err := os.ReadFile(Path("decisionLog.md")); err != nil || string(b) != body {
				t.Fatalf("existing data changed: %q, err=%v", b, err)
			}
		})
	}
}

func TestStampLayoutRefusesMalformedAndUnreadable(t *testing.T) {
	for _, stamp := range []string{"", " \n", "flat +4", "flat -1", "flat 0", "flat nope", "flat 4 extra", "other 4", "directory"} {
		t.Run(stamp, func(t *testing.T) {
			isolatedConfig(t)
			if stamp == "directory" {
				write(t, filepath.Join(Path(layoutFile), "keep"), "stored data")
			} else {
				write(t, Path(layoutFile), stamp)
			}
			ran := false
			err := withBankLock(BankLockWait, func() error { ran = true; return nil })
			if err == nil || ran {
				t.Fatalf("refusal err=%v, writer ran=%t", err, ran)
			}
			p, want := Path(layoutFile), stamp
			if stamp == "directory" {
				p, want = filepath.Join(p, "keep"), "stored data"
			}
			if b, err := os.ReadFile(p); err != nil || string(b) != want {
				t.Fatalf("refused stamp changed: %q, err=%v", b, err)
			}
		})
	}
}

func TestStampLayoutWaitsForWriterLock(t *testing.T) {
	isolatedConfig(t)
	write(t, Path(layoutFile), "flat 3\n")
	if err := WithBankLock(BankLockWait, func() error {
		err := withBankLock(time.Millisecond, func() error { t.Error("writer ran without lock"); return nil })
		if !errors.Is(err, ErrBankBusy) {
			t.Errorf("busy writer err=%v", err)
		}
		if b, err := os.ReadFile(Path(layoutFile)); err != nil || string(b) != "flat 3\n" {
			t.Errorf("busy writer changed stamp: %q, err=%v", b, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
