package memory

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestNativeRegressionZeroByteRoundTrip(t *testing.T) {
	tempConfig(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "empty.md"), "")
	if _, err := ArchiveNative(dir, []string{"empty"}, true); err != nil {
		t.Fatal(err)
	}
	assertNativeRegressionEmptyMove(t, nativeArchiveDir(dir), dir)
	if _, err := RestoreNative(dir, []string{"empty"}, true); err != nil {
		t.Fatal(err)
	}
	assertNativeRegressionEmptyMove(t, dir, nativeArchiveDir(dir))
}

func assertNativeRegressionEmptyMove(t *testing.T, destination, source string) {
	t.Helper()
	info, err := os.Stat(filepath.Join(destination, "empty.md"))
	if err != nil {
		t.Fatalf("empty destination missing: %v", err)
	}
	if !info.Mode().IsRegular() || info.Size() != 0 {
		t.Fatalf("destination is not an empty regular file: %v", info)
	}
	if _, err := os.Lstat(filepath.Join(source, "empty.md")); !os.IsNotExist(err) {
		t.Fatalf("source still present or inaccessible: %v", err)
	}
}

func TestNativeRegressionMissingIndexFindings(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "orphan.md"), "body\n")
	write(t, filepath.Join(dir, "linked.md"), "body\n")
	write(t, filepath.Join(dir, NativeSearchableFile), "- [Linked](linked.md)\n- [Gone](gone.md)\n")
	st, err := NativeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(st.Orphans, ",") != "orphan.md" || strings.Join(st.Dangling, ",") != "gone.md" {
		t.Fatalf("missing MEMORY.md hid findings: %+v", st)
	}
	var out bytes.Buffer
	if err := nativeStatsReport(&out, dir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "orphans:   1 topic file no pointer reaches: orphan.md") ||
		!strings.Contains(out.String(), "dangling:  1 pointer with no topic file: gone.md") {
		t.Fatalf("findings missing from report:\n%s", &out)
	}
}

func TestNativeRegressionBatchRestoreCoreCap(t *testing.T) {
	tempConfig(t)
	dir := t.TempDir()
	var core strings.Builder
	for i := range 19 {
		name := fmt.Sprintf("core-%02d.md", i)
		fmt.Fprintf(&core, "- [Core](%s)\n", name)
		write(t, filepath.Join(dir, name), "body\n")
	}
	write(t, filepath.Join(dir, NativeIndexFile), core.String())
	archive := nativeArchiveDir(dir)
	write(t, filepath.Join(archive, "first.md"), "first body\n")
	write(t, filepath.Join(archive, "second.md"), "second body\n")
	write(t, filepath.Join(archive, NativeArchiveIndexFile), "- [First](first.md)\n- [Second](second.md)\n")
	res, err := RestoreNative(dir, []string{"first", "second"}, true)
	if err != nil {
		t.Fatal(err)
	}
	index := mustRead(t, filepath.Join(dir, NativeIndexFile))
	searchable := mustRead(t, filepath.Join(dir, NativeSearchableFile))
	if len(indexLinks(index)) != 20 || !strings.Contains(index, "(first.md)") || strings.Contains(index, "(second.md)") {
		t.Fatalf("batch restore exceeded or underfilled core: %s", index)
	}
	if len(indexLinks(searchable)) != 1 || !strings.Contains(searchable, "(second.md)") {
		t.Fatalf("overflow missing or duplicated: %s", searchable)
	}
	assertNativeRegressionRestoredFiles(t, dir, res)
}

func assertNativeRegressionRestoredFiles(t *testing.T, dir string, res NativeResult) {
	t.Helper()
	if len(res.Restored) != 2 {
		t.Fatalf("batch did not restore both files: %+v", res)
	}
	for _, name := range []string{"first", "second"} {
		if got := mustRead(t, filepath.Join(dir, name+".md")); got != name+" body\n" {
			t.Errorf("restored %s bytes = %q", name, got)
		}
	}
}

func TestNativeRegressionLstatError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink("loop", dir); err != nil {
		t.Fatal(err)
	}
	for _, run := range []func(string, []string, bool) (NativeResult, error){ArchiveNative, RestoreNative} {
		res, err := run(dir, []string{"topic"}, false)
		if !errors.Is(err, syscall.ELOOP) {
			t.Errorf("Lstat I/O error became absence: result=%+v err=%v", res, err)
		}
	}
}

func TestNativeRegressionSearchableReadError(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, NativeIndexFile), "# Index\n")
	write(t, filepath.Join(dir, "topic.md"), "body\n")
	if err := os.Mkdir(filepath.Join(dir, NativeSearchableFile), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := NativeStats(dir)
	if !errors.Is(err, syscall.EISDIR) || len(st.Orphans) != 0 || len(st.Dangling) != 0 {
		t.Fatalf("NativeStats fabricated findings instead of a read error: stats=%+v err=%v", st, err)
	}
	dangling, orphans, err := indexFaults(dir, "", map[string]bool{"topic.md": true})
	if !errors.Is(err, syscall.EISDIR) || len(orphans) != 0 || len(dangling) != 0 {
		t.Fatalf("indexFaults fabricated findings: dangling=%v orphans=%v err=%v", dangling, orphans, err)
	}
}

func TestNativeRegressionRestoreReportDestination(t *testing.T) {
	dir := nativeStore(t)
	if _, err := ArchiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	err := ArchiveReport(&out, &errout, ArchiveReportOptions{
		Dir: dir, Names: []string{"stale-fact"}, Apply: true, Restore: true, Split: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "restored 1 entry -> " + dir + "\n   stale-fact.md\n"
	if out.String() != want || errout.Len() != 0 {
		t.Fatalf("restore destination report: stdout=%q stderr=%q, want %q", &out, &errout, want)
	}
	if !fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Fatal("reported restore did not land in the live store")
	}
}
