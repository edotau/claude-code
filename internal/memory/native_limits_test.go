package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSplitReportsNativeProseOverLimit: the split moves what it can and then reports an index the native loader
// would still truncate instead of silently rewriting the prose that busts it.
func TestSplitReportsNativeProseOverLimit(t *testing.T) {
	dir := nativeStore(t)
	indexPath := filepath.Join(dir, NativeIndexFile)
	prose := strings.Repeat("prose that is not a pointer\n", NativeIndexMaxLines)
	write(t, indexPath, prose+testIndex)
	moved, err := SplitNativeIndex(dir, 1)
	if moved != 2 {
		t.Errorf("moved = %d, want 2", moved)
	}
	if err == nil || !strings.Contains(err.Error(), "native load limit") {
		t.Errorf("err = %v, want the native load-limit diagnostic", err)
	}
	got := mustRead(t, indexPath)
	if !strings.HasPrefix(got, prose) || strings.Contains(got, "stale-fact.md") || !strings.Contains(got, "also-keep.md") {
		t.Errorf("split rewrote prose or kept the wrong pointers:\n%s", got[len(got)-200:])
	}
	if _, err := SplitNativeIndex(dir, 1); err == nil || !strings.Contains(err.Error(), "native load limit") {
		t.Errorf("no-op re-run err = %v, want the same diagnostic", err)
	}
}

// TestRestoreNativeReportsIndexOverLimit: a restore whose pointer pushes MEMORY.md past the native load
// limit still lands, and the error names the limit so the overflow is never silent.
func TestRestoreNativeReportsIndexOverLimit(t *testing.T) {
	dir := nativeStore(t)
	if _, err := ArchiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatal(err)
	}
	idxPath := filepath.Join(dir, NativeIndexFile)
	idx := mustRead(t, idxPath)
	write(t, idxPath, idx+strings.Repeat("prose\n", NativeIndexMaxLines-LineCount(idx)))
	res, err := RestoreNative(dir, []string{"stale-fact"}, true)
	if err == nil || !strings.Contains(err.Error(), "exceed the native load limit") {
		t.Fatalf("RestoreNative err = %v, want the load-limit diagnostic", err)
	}
	if len(res.Restored) != 1 || !fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Errorf("restore did not land: Restored = %v", res.Restored)
	}
	if got := mustRead(t, idxPath); !strings.Contains(got, "(stale-fact.md)") || LineCount(got) != NativeIndexMaxLines+1 {
		t.Errorf("index has %d lines, want %d with the restored pointer", LineCount(got), NativeIndexMaxLines+1)
	}
}

// TestArchiveNativeReportsHealedIndexOverLimit: the reconcile repair riding an archive can grow MEMORY.md past
// the native load limit; the archive lands and the error names the limit.
func TestArchiveNativeReportsHealedIndexOverLimit(t *testing.T) {
	dir := nativeStore(t)
	for _, n := range []string{"orphan-one.md", "orphan-two.md"} {
		write(t, filepath.Join(dir, n), "---\nname: "+strings.TrimSuffix(n, ".md")+"\n---\n\nbody\n")
	}
	idxPath := filepath.Join(dir, NativeIndexFile)
	write(t, idxPath, testIndex+strings.Repeat("prose\n", NativeIndexMaxLines-LineCount(testIndex)))
	res, err := ArchiveNative(dir, []string{"stale-fact"}, true)
	if err == nil || !strings.Contains(err.Error(), "exceed the native load limit") {
		t.Fatalf("ArchiveNative err = %v, want the load-limit diagnostic", err)
	}
	if len(res.Archived) != 1 || fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Errorf("archive did not land: Archived = %v", res.Archived)
	}
	assertHealedArchiveIndexOverLimit(t, idxPath)
}

func assertHealedArchiveIndexOverLimit(t *testing.T, idxPath string) {
	t.Helper()
	got := mustRead(t, idxPath)
	if strings.Contains(got, "(stale-fact.md)") || !strings.Contains(got, "(orphan-one.md)") || !strings.Contains(got, "(orphan-two.md)") {
		t.Errorf("index not struck and healed:\n%s", got)
	}
	if LineCount(got) != NativeIndexMaxLines+1 {
		t.Errorf("index has %d lines, want %d", LineCount(got), NativeIndexMaxLines+1)
	}
}

// TestHealedIndexUnreadableIsAnError: a MEMORY.md that exists but cannot be read must abort, not look absent.
func TestHealedIndexUnreadableIsAnError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, NativeIndexFile), 0o755); err != nil { // reads fail with EISDIR
		t.Fatal(err)
	}
	if _, _, err := healedIndex(dir); err == nil {
		t.Fatal("healedIndex: want a read error, got nil")
	}
	if raw, _, err := healedIndex(t.TempDir()); err != nil || raw != nil {
		t.Fatalf("absent index: raw=%v err=%v, want nil, nil", raw, err)
	}
}
