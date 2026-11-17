package memory

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// importTar builds a tar of name->body entries; a body starting "->" is a symlink to the rest.
func importTar(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if link, ok := strings.CutPrefix(body, "->"); ok {
			h = &tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: link}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(body))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// wantFile asserts path holds want; an empty want means the file must not exist.
func wantFile(t *testing.T, path, want string) {
	t.Helper()
	if !pathExists(path) {
		if want != "" {
			t.Errorf("%s missing", path)
		}
		return
	}
	if got := aliasRead(t, path); got != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

func TestImportRefusesSourceWithDifferentLayout(t *testing.T) {
	for name, stamp := range map[string]string{"unstamped": "", "flat 2": layoutFlat2 + "\n"} {
		t.Run(name, func(t *testing.T) {
			tempConfig(t)
			from := t.TempDir()
			if stamp != "" {
				write(t, filepath.Join(from, layoutFile), stamp)
			}
			_, err := ImportBank(ImportOptions{From: from, DryRun: true})
			var in *ImportInputError
			if !errors.As(err, &in) || !strings.Contains(err.Error(), "older store layout") {
				t.Errorf("import of a %s source err = %v, want the migrate hint", name, err)
			}
			if _, err := os.Stat(filepath.Join(StoreDir(), layoutFile)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("import --dry-run stamped the store layout (stat err = %v); it must write nothing", err)
			}
		})
	}
	tempConfig(t)
	from := t.TempDir()
	write(t, filepath.Join(StoreDir(), "alpha", "progress.md"), "x\n")
	write(t, filepath.Join(from, layoutFile), layoutFlat+"\n")
	if _, err := ImportBank(ImportOptions{From: from, DryRun: true}); !errors.Is(err, errStoreNotFlat) {
		t.Errorf("import into a per-repo store err = %v, want errStoreNotFlat", err)
	}
}

func flatImportFixture(t *testing.T) string {
	t.Helper()
	isolatedConfig(t)
	initStore(t)
	write(t, Path("decisionLog.md"), "# Log\n\n## 2026-11-01 — local <!-- id:11111111 -->\nLocal body.\n")
	write(t, NativeIndex(), "- [Local](local.md)\n")
	write(t, Path("topic.md"), "keep local\n")
	src := t.TempDir()
	files := map[string]string{
		layoutFile: layoutFlat + "\n", "decisionLog.md": "# Log\n\n## 2026-11-02 — remote <!-- id:22222222 -->\nRemote body.\n",
		"topic.md": "remote conflict\n", "new.md": "new topic\n", "MEMORY.md": "## Old header\n- [Remote](new.md)\n",
		catalogFile: "foreign catalog\n", "writer.lock": "lock\n", ".git/config": "foreign git\n",
		"archive/decisionLog-2026-10.md": "## 2026-10-01 — archived <!-- id:44444444 -->\n",
		"native-only.md":                 "shared topic\n", "reports/bundle/report.md": "nested report\n", "locks/writer.md": "lock metadata\n",
	}
	for rel, body := range files {
		write(t, filepath.Join(src, rel), body)
	}
	write(t, filepath.Join(src, "huge.md"), strings.Repeat("x", importMaxFile+1))
	if err := os.Symlink("new.md", filepath.Join(src, "link.md")); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestImportFlatAdditiveAndIdempotent(t *testing.T) {
	src := flatImportFixture(t)
	o := ImportOptions{From: src, Topics: true}
	res, err := ImportBank(o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Summary, "1 conflict(s)") {
		t.Fatalf("%+v", res)
	}
	log := aliasRead(t, Path("decisionLog.md"))
	if !strings.Contains(log, "11111111") || !strings.Contains(log, "22222222") {
		t.Fatal(log)
	}
	wantFile(t, Path("topic.md"), "keep local\n")
	wantFile(t, Path("new.md"), "new topic\n")
	wantFile(t, filepath.Join(StoreDir(), "native-only.md"), "shared topic\n")
	wantFile(t, Sub("reports", "bundle/report.md"), "nested report\n")
	if idx := aliasRead(t, NativeIndex()); !strings.Contains(idx, "(new.md)") || strings.Contains(idx, "## ") {
		t.Fatalf("MEMORY.md = %q, want the remote pointer and no header", idx)
	}
	for _, rel := range []string{"writer.lock", "huge.md", "link.md", "locks/writer.md"} {
		if pathExists(filepath.Join(StoreDir(), rel)) {
			t.Errorf("imported excluded file %s", rel)
		}
	}
	if strings.Contains(aliasRead(t, filepath.Join(StoreDir(), catalogFile)), "foreign catalog") {
		t.Fatal("imported catalog")
	}
	res, err = ImportBank(o)
	if err != nil || res.Summary != "nothing to import" {
		t.Fatalf("repeat: %+v, %v", res, err)
	}
}

func TestImportFlatKeyLogsOnlyAndDryRun(t *testing.T) {
	src := flatImportFixture(t)
	o := ImportOptions{From: src, DryRun: true}
	res, err := ImportBank(o)
	if err != nil || !strings.Contains(res.Summary, "would import") {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Contains(aliasRead(t, Path("decisionLog.md")), "22222222") {
		t.Fatal("dry run wrote")
	}
	o.DryRun = false
	if _, err := ImportBank(o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(aliasRead(t, Path("decisionLog.md")), "22222222") {
		t.Fatal("missing remote log")
	}
	if pathExists(Path("new.md")) || strings.Contains(aliasRead(t, NativeIndex()), "(new.md)") {
		t.Fatal("topics imported without flag")
	}
}

func TestImportRefusesCredentialShapedSourceBeforeWriting(t *testing.T) {
	key := "sk-" + strings.Repeat("a", 40) // built at runtime so no literal key sits in the source
	for _, tc := range []struct{ name, rel, body string }{
		{"copy", "reports/leak.md", "k=" + key + "\n"},
		{"merge", "decisionLog.md", "## 2026-11-03 — leak <!-- id:55555555 -->\nuses " + key + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := flatImportFixture(t)
			write(t, filepath.Join(src, tc.rel), tc.body)
			write(t, filepath.Join(src, "topic.md"), key+"\n") // a kept-local conflict is not a blocker
			// Stamp first: a live store already carries .layout, which the first lock would otherwise write.
			if err := requireFlat(); err != nil {
				t.Fatal(err)
			}
			before := dirHashes(t, StoreDir())
			for _, dry := range []bool{true, false} {
				_, err := ImportBank(ImportOptions{From: src, Topics: true, DryRun: dry})
				if err == nil || !strings.Contains(err.Error(), "credential-shaped content: "+tc.rel) ||
					!strings.Contains(err.Error(), "nothing was imported") || strings.Contains(err.Error(), "topic.md") {
					t.Fatalf("dry=%v: err = %v, want a refusal naming only %s", dry, err, tc.rel)
				}
				if !equalSnap(before, dirHashes(t, StoreDir())) {
					t.Fatalf("dry=%v: a refused import changed the local store", dry)
				}
			}
		})
	}
}

func TestExtractStoreTarKeepsFlatInputs(t *testing.T) {
	dir := t.TempDir()
	_, err := ExtractStoreTar(importTar(t, map[string]string{layoutFile: layoutFlat + "\n", "alpha--decisionLog.md": "entry\n", nativeIndexFile: "pointer\n", catalogFile: "catalog\n", "alpha--writer.lock": "lock\n", ".git/config": "git\n"}), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{layoutFile, "alpha--decisionLog.md", nativeIndexFile} {
		if !pathExists(filepath.Join(dir, rel)) {
			t.Errorf("missing %s from host archive", rel)
		}
	}
	for _, rel := range []string{catalogFile, "alpha--writer.lock", ".git/config"} {
		if pathExists(filepath.Join(dir, rel)) {
			t.Errorf("extracted excluded %s", rel)
		}
	}
}

func aliasRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func pathExists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestMergeNativeIndexDedupesByTargetAndKeepsHeadersOut(t *testing.T) {
	tgt := "- [Old title](a.md) — old hook\n\n## Notes\n- [N](n.md)\n"
	src := "## Remote header\n- [New title](a.md) — new hook\n- [B](b.md)\n"
	got, n, err := mergeNativeIndex(tgt, src)
	want := "- [Old title](a.md) — old hook\n- [B](b.md)\n\n## Notes\n- [N](n.md)\n"
	if err != nil || n != 1 || got != want {
		t.Errorf("got %q n=%d err=%v, want %q n=1", got, n, err, want)
	}
	if got, _, _ := mergeNativeIndex("\n## H\n", "- [B](b.md)\n"); got != "\n- [B](b.md)\n## H\n" {
		t.Errorf("leading blank + header: got %q", got)
	}
}

func TestBlocksMissingFromUsesFullIdentity(t *testing.T) {
	tgt := []string{"## 2026-11-01 — same <!-- id:11111111 -->\nbody\n"}
	for name, c := range map[string]struct {
		src  string
		want int
	}{
		"other id, same text":   {"## 2026-11-01 — same <!-- id:22222222 -->\nbody\n", 1},
		"same id, edited text":  {"## 2026-11-01 — same <!-- id:11111111 -->\nedited\n", 0},
		"no id, same text":      {"## 2026-11-01 — same\nbody\n", 0},
		"no id, different text": {"## 2026-11-01 — same\nother\n", 1},
	} {
		if got := blocksMissingFrom(tgt, []string{c.src}); len(got) != c.want {
			t.Errorf("%s: added %d, want %d", name, len(got), c.want)
		}
	}
}

func TestFlatArchiveNames(t *testing.T) {
	if !monthlyArchive("archive/decisionLog-2026-10.md") || monthlyArchive("decisionLog-2026-10.md") {
		t.Error("monthlyArchive must match the archive-- prefix only")
	}
	tgt := t.TempDir()
	write(t, filepath.Join(tgt, "archive/decisionLog-2026-10.md"), "## 2026-10-01 — old <!-- id:44444444 -->\n")
	src := []byte("## 2026-10-01 — old <!-- id:44444444 -->\n\n## 2026-11-01 — new <!-- id:55555555 -->\n")
	if got := string(unarchived(tgt, "decisionLog.md", src)); strings.Contains(got, "44444444") || !strings.Contains(got, "55555555") {
		t.Errorf("unarchived = %q", got)
	}
}
