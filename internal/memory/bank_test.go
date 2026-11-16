package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
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

func TestBankOfFileNestedBundle(t *testing.T) {
	isolatedConfig(t)
	setStoreFlat(t, true)
	migrateWrite(t, "alpha--progress.md", "p\n")
	for _, name := range []string{"reports/alpha--bundle/sub/review.md", "archive/alpha--dream-backup/day/MEMORY.md"} {
		for _, path := range []string{name, filepath.Join(StoreDir(), name)} {
			if got := BankOfFile(path); got.Key != "alpha" {
				t.Errorf("BankOfFile(%s) = %+v", path, got)
			}
		}
	}
}

func TestBankKeysAndOwnerSplitOnFirstDoubleDash(t *testing.T) {
	isolatedConfig(t)
	setStoreFlat(t, true)
	s := StoreDir()
	for _, n := range []string{"my--progress.md", "my--repo--progress.md"} {
		write(t, filepath.Join(s, n), "x\n")
	}
	if got := BankKeys(); len(got) != 1 || got[0] != "my" {
		t.Fatalf("BankKeys = %v, want [my]", got)
	}
	for file, want := range map[string]string{"my--repo--topic.md": "my", "my--topic.md": "my", "reports/my--repo--r.md": "my", "other--x.md": ""} {
		if got := BankOfFile(filepath.Join(s, filepath.FromSlash(file))); got.Key != want {
			t.Errorf("BankOfFile(%s) = %q, want %q", file, got.Key, want)
		}
	}
}

func TestBankForCollapsesDashRunsAndSplitsSameBasenameRepos(t *testing.T) {
	cfg := tempConfig(t)
	setStoreFlat(t, true)
	a := gitRepo(t, t.TempDir(), "my--api")
	hosttest.In(t, a, "remote", "add", "origin", "https://github.com/org-a/my--api.git")
	b := gitRepo(t, t.TempDir(), "x")
	hosttest.In(t, b, "remote", "add", "origin", "https://github.com/org-b/my--api.git")
	if got := BankFor(a).Key; got != "my-api" {
		t.Fatalf("BankFor key = %q, want my-api", got)
	}
	if _, err := EnsureBank(a, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureBank(a, ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(cfg, "memory", ".origins"))
	if want := "my-api\thttps://github.com/org-a/my--api.git\n"; string(data) != want {
		t.Fatalf(".origins = %q, want %q", data, want)
	}
	kb := BankFor(b).Key
	if kb == "my-api" || !strings.HasPrefix(kb, "my-api-") || len(kb) != len("my-api-")+8 {
		t.Fatalf("second origin's key = %q, want my-api-<8 hex>", kb)
	}
	if got := BankFor(a).Key; got != "my-api" {
		t.Errorf("first origin's key moved to %q", got)
	}
}

func TestPrefixIsEmptyOnLegacyLayout(t *testing.T) {
	isolatedConfig(t)
	b := Bank{Key: "alpha"}
	setStoreFlat(t, false)
	if got := b.Prefix(); got != "" {
		t.Errorf("legacy Prefix = %q, want empty", got)
	}
	setStoreFlat(t, true)
	if got := b.Prefix(); got != "alpha--" {
		t.Errorf("flat Prefix = %q, want alpha--", got)
	}
}
