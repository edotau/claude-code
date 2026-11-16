package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// setStoreFlat forces the store layout for one test, restoring the previous force on cleanup.
func setStoreFlat(tb testing.TB, flat bool) {
	tb.Helper()
	prev := flatForced.Load()
	v := int32(1)
	if flat {
		v = 2
	}
	flatForced.Store(v)
	tb.Cleanup(func() { flatForced.Store(prev) })
}

func TestBankPaths(t *testing.T) {
	isolatedConfig(t)
	s := StoreDir()
	for _, tc := range []struct {
		key, legacy, flatPath, flatSub string
	}{
		{"claude-code", "claude-code", "claude-code--activeContext.md", "reports/claude-code--x.md"},
		{"", "home", "activeContext.md", "reports/x.md"},
		{"digitalbtd-rapid", "digitalbtd-rapid", "digitalbtd-rapid--activeContext.md", "reports/digitalbtd-rapid--x.md"},
	} {
		b := Bank{Key: tc.key}
		setStoreFlat(t, false)
		if got, want := b.Path("activeContext.md"), filepath.Join(s, tc.legacy, "activeContext.md"); got != want {
			t.Errorf("legacy Path(%q) = %s, want %s", tc.key, got, want)
		}
		if got, want := b.Sub("reports", "x.md"), filepath.Join(s, tc.legacy, "reports", "x.md"); got != want {
			t.Errorf("legacy Sub(%q) = %s, want %s", tc.key, got, want)
		}
		if got, want := b.NativeIndex(), filepath.Join(s, tc.legacy, "MEMORY.md"); got != want {
			t.Errorf("legacy NativeIndex(%q) = %s, want %s", tc.key, got, want)
		}
		setStoreFlat(t, true)
		if got, want := b.Path("activeContext.md"), filepath.Join(s, tc.flatPath); got != want {
			t.Errorf("flat Path(%q) = %s, want %s", tc.key, got, want)
		}
		if got, want := b.Sub("reports", "x.md"), filepath.Join(s, filepath.FromSlash(tc.flatSub)); got != want {
			t.Errorf("flat Sub(%q) = %s, want %s", tc.key, got, want)
		}
		if got, want := b.NativeIndex(), filepath.Join(s, "MEMORY.md"); got != want {
			t.Errorf("flat NativeIndex(%q) = %s, want %s", tc.key, got, want)
		}
	}
}

func TestBankOfFileFlat(t *testing.T) {
	isolatedConfig(t)
	setStoreFlat(t, true)
	s := StoreDir()
	if err := os.MkdirAll(s, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"claude-code--decisionLog.md", "digitalbtd-rapid--progress.md", "decisionLog.md", "x--notes.md"} {
		if err := os.WriteFile(filepath.Join(s, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := BankKeys(); len(got) != 2 || got[0] != "claude-code" || got[1] != "digitalbtd-rapid" {
		t.Fatalf("BankKeys = %v, want [claude-code digitalbtd-rapid]", got)
	}
	if got := BankOfFile(filepath.Join(s, "reports", "claude-code--x.md")); got.Key != "claude-code" {
		t.Errorf("BankOfFile(claude-code--x.md) = %q", got.Key)
	}
	if got := BankOfFile(filepath.Join(s, "reports", "unknown--x.md")); !got.Home() {
		t.Errorf("unknown prefix = %q, want home", got.Key)
	}
	if got := Banks(); len(got) != 3 || !got[0].Home() {
		t.Errorf("Banks = %v, want home first then 2 keys", got)
	}
}

func TestStoreFlatReadsStamp(t *testing.T) {
	isolatedConfig(t)
	if storeFlat() {
		t.Fatal("fresh store reads flat")
	}
	if err := os.MkdirAll(StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(StoreDir(), layoutFile), []byte(layoutFlat+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	forgetLayout()
	if !storeFlat() {
		t.Fatal("stamped store reads legacy")
	}
}
