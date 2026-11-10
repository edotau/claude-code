package memory

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

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
		"reconcileLines": func() error { _, err := reconcileLines(dir, ""); return err },
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
