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
	if _, err := archiveNative(dir, []string{"empty"}, true); err != nil {
		t.Fatal(err)
	}
	assertNativeRegressionEmptyMove(t, nativeArchiveDir(dir), dir)
	if _, err := restoreNative(dir, []string{"empty"}, true); err != nil {
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
	st, err := nativeStats(dir)
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
	res, err := restoreNative(dir, []string{"first", "second"}, true)
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

func assertNativeRegressionRestoredFiles(t *testing.T, dir string, res nativeResult) {
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
	for _, run := range []func(string, []string, bool) (nativeResult, error){archiveNative, restoreNative} {
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
	st, err := nativeStats(dir)
	if !errors.Is(err, syscall.EISDIR) || len(st.Orphans) != 0 || len(st.Dangling) != 0 {
		t.Fatalf("nativeStats fabricated findings instead of a read error: stats=%+v err=%v", st, err)
	}
	dangling, orphans, err := indexFaults(dir, "", map[string]bool{"topic.md": true})
	if !errors.Is(err, syscall.EISDIR) || len(orphans) != 0 || len(dangling) != 0 {
		t.Fatalf("indexFaults fabricated findings: dangling=%v orphans=%v err=%v", dangling, orphans, err)
	}
}

func TestNativeRegressionRestoreReportDestination(t *testing.T) {
	dir := nativeStore(t)
	if _, err := archiveNative(dir, []string{"stale-fact"}, true); err != nil {
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

// TestArchiveRestoreSkipMissingTopics: a name absent from the store (archive) or archive (restore) is skipped with its reason.
func TestArchiveRestoreSkipMissingTopics(t *testing.T) {
	dir := nativeStore(t)
	res, err := archiveNative(dir, []string{"stale-fact", "ghost"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 1 || len(res.Skipped) != 1 || res.Skipped[0] != "ghost.md: not in the store" {
		t.Errorf("archive = %+v, want stale-fact archived and ghost.md skipped as not in the store", res)
	}
	res, err = restoreNative(dir, []string{"stale-fact", "ghost"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restored) != 1 || len(res.Skipped) != 1 || res.Skipped[0] != "ghost.md: not in the archive" {
		t.Errorf("restore = %+v, want stale-fact planned and ghost.md skipped as not in the archive", res)
	}
}

// TestArchiveKeepsSkippedOnRefusal: a later refused name returns the skips gathered before it.
func TestArchiveKeepsSkippedOnRefusal(t *testing.T) {
	dir := nativeStore(t)
	res, err := archiveNative(dir, []string{"ghost", "MEMORY.md"}, true)
	if err == nil {
		t.Fatal("an index name must be refused")
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "ghost.md: not in the store" {
		t.Errorf("Skipped = %v, want the ghost skip kept alongside the error", res.Skipped)
	}
}

func TestNativeErrorRegressionRestoreMissingIndexIdenticalLive(t *testing.T) {
	tempConfig(t)
	dir := t.TempDir()
	archive := nativeArchiveDir(dir)
	const body = "identical topic body without frontmatter\n"
	const pointer = "- [Recorded title](topic.md) — preserve this exact description"
	write(t, filepath.Join(dir, "topic.md"), body)
	write(t, filepath.Join(archive, "topic.md"), body)
	write(t, filepath.Join(archive, NativeArchiveIndexFile), pointer+"\n")

	res, err := restoreNative(dir, []string{"topic"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Restored, ",") != "topic.md" {
		t.Fatalf("restore did not succeed: %+v", res)
	}
	if got := mustRead(t, filepath.Join(dir, NativeIndexFile)); strings.TrimSpace(got) != pointer {
		t.Fatalf("recorded pointer lost or rewritten after identical-live restore: %q", got)
	}
	if got := mustRead(t, filepath.Join(dir, "topic.md")); got != body {
		t.Fatalf("identical live copy changed: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(archive, "topic.md")); !os.IsNotExist(err) {
		t.Fatalf("archived copy was not removed: %v", err)
	}
	if got := mustRead(t, filepath.Join(archive, NativeArchiveIndexFile)); strings.Contains(got, "topic.md") {
		t.Fatalf("restored pointer still recorded in archive: %q", got)
	}
}

func TestNativeErrorRegressionDirectoryReadFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read permissions")
	}
	dir := t.TempDir()
	// Keep lookup permission so absent index reads succeed before ReadDir fails.
	if err := os.Chmod(dir, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	for name, run := range map[string]func() error{
		"nativeStats":    func() error { _, err := nativeStats(dir); return err },
		"reconcileLines": func() error { _, _, err := reconcileLines(dir, "", nil); return err },
	} {
		err := run()
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			t.Errorf("%s swallowed directory read failure: %v", name, err)
			continue
		}
		if pathErr.Path != dir || !errors.Is(err, syscall.EACCES) {
			t.Errorf("%s did not propagate the directory read error: %v", name, err)
		}
	}
}

func TestNativeErrorRegressionUnreadableTopicDupes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses topic read permissions")
	}
	dir := t.TempDir()
	topic := filepath.Join(dir, "unreadable.md")
	write(t, topic, "topic body\n")
	if err := os.Chmod(topic, 0); err != nil {
		t.Fatal(err)
	}
	clusters, err := nativeDupes(dir, dupeThreshold)
	if !errors.Is(err, os.ErrPermission) || len(clusters) != 0 {
		t.Errorf("unreadable topic was silently skipped: clusters=%v err=%v", clusters, err)
	}
	var out, errout bytes.Buffer
	err = ArchiveReport(&out, &errout, ArchiveReportOptions{Dir: dir, Dupes: true, Split: -1})
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("dupes report hid unreadable topic: err=%v stdout=%q", err, &out)
	}
	if out.Len() != 0 {
		t.Errorf("dupes report claimed success after incomplete scan: %q", &out)
	}
}

// readIfExists returns a file's bytes, nil for an absent file, and every other read error.
func TestReadIfExists(t *testing.T) {
	dir := t.TempDir()
	if b, err := readIfExists(dir); !errors.Is(err, syscall.EISDIR) || len(b) != 0 {
		t.Errorf("directory: %q, %v; want the EISDIR read error", b, err)
	}
	if b, err := readIfExists(filepath.Join(dir, "nope")); b != nil || err != nil {
		t.Errorf("absent: %q, %v; want nil, nil", b, err)
	}
	write(t, filepath.Join(dir, "topic.md"), "body\n")
	if b, err := readIfExists(filepath.Join(dir, "topic.md")); string(b) != "body\n" || err != nil {
		t.Errorf("present: %q, %v; want its bytes", b, err)
	}
}
