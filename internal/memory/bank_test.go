package memory

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

func TestBankPaths(t *testing.T) {
	isolatedConfig(t)
	s := StoreDir()
	for _, tc := range []struct{ key, path, sub string }{
		{"claude-code", "claude-code--activeContext.md", "reports/claude-code--x.md"},
		{"", "activeContext.md", "reports/x.md"},
		{"digitalbtd-rapid", "digitalbtd-rapid--activeContext.md", "reports/digitalbtd-rapid--x.md"},
	} {
		b := Bank{Key: tc.key}
		if got, want := b.Path("activeContext.md"), filepath.Join(s, tc.path); got != want {
			t.Errorf("Path(%q) = %s, want %s", tc.key, got, want)
		}
		if got, want := b.Sub("reports", "x.md"), filepath.Join(s, filepath.FromSlash(tc.sub)); got != want {
			t.Errorf("Sub(%q) = %s, want %s", tc.key, got, want)
		}
		if got, want := b.NativeIndex(), filepath.Join(s, "MEMORY.md"); got != want {
			t.Errorf("NativeIndex(%q) = %s, want %s", tc.key, got, want)
		}
	}
}

func TestBankOfFileFlat(t *testing.T) {
	isolatedConfig(t)
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
		t.Fatal("unstamped store reads flat")
	}
	write(t, filepath.Join(StoreDir(), layoutFile), layoutFlat+"\n")
	forgetLayout()
	if !storeFlat() {
		t.Fatal("stamped store reads legacy")
	}
}

func TestBankKeysIncludesRecordedReportOnlyBank(t *testing.T) {
	isolatedConfig(t)
	write(t, filepath.Join(StoreDir(), layoutFile), layoutFlat+"\n")
	forgetLayout()
	write(t, filepath.Join(StoreDir(), originsFile), "solo\t/src/solo\n")
	if got := BankKeys(); !slices.Contains(got, "solo") {
		t.Fatalf("BankKeys = %v, want the recorded report-only bank solo", got)
	}
}

func TestRequireFlatSeesLegacyBankNamedLikeASharedSub(t *testing.T) {
	isolatedConfig(t)
	write(t, filepath.Join(StoreDir(), "reports", "progress.md"), "x\n")
	if err := requireFlat(); !errors.Is(err, errStoreNotFlat) || storeFlat() {
		t.Fatalf("requireFlat = %v (flat=%v), want errStoreNotFlat and no stamp", err, storeFlat())
	}
}

func TestWritersRefuseLegacyStoreAndStampFreshOne(t *testing.T) {
	isolatedConfig(t)
	write(t, filepath.Join(StoreDir(), "alpha", "progress.md"), "x\n")
	root := gitRepo(t, t.TempDir(), "proj")
	if _, err := EnsureBank(root, ""); !errors.Is(err, errStoreNotFlat) {
		t.Errorf("EnsureBank = %v, want errStoreNotFlat", err)
	}
	if _, err := Init(root); !errors.Is(err, errStoreNotFlat) {
		t.Errorf("Init = %v, want errStoreNotFlat", err)
	}
	if err := withFlatLock(BankLockWait, func() error { return nil }); !errors.Is(err, errStoreNotFlat) {
		t.Errorf("withFlatLock = %v, want errStoreNotFlat", err)
	}
	if got := BankKeys(); len(got) != 0 {
		t.Errorf("BankKeys on a legacy store = %v, want none", got)
	}
	if err := os.RemoveAll(filepath.Join(StoreDir(), "alpha")); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(root); err != nil || !storeFlat() {
		t.Errorf("Init on a fresh store = %v, flat=%v, want it stamped", err, storeFlat())
	}
}

func TestBankOfFileNestedBundle(t *testing.T) {
	isolatedConfig(t)
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

func TestPrefix(t *testing.T) {
	if got := (Bank{Key: "alpha"}).Prefix(); got != "alpha--" {
		t.Errorf("Prefix = %q, want alpha--", got)
	}
	if got := HomeBank().Prefix(); got != "" {
		t.Errorf("home Prefix = %q, want empty", got)
	}
}

func migrateWrite(t *testing.T, rel, text string) {
	t.Helper()
	if err := paths.AtomicWrite(filepath.Join(StoreDir(), filepath.FromSlash(rel)), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
