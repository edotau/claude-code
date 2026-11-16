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
	tempConfig(t)
	from := t.TempDir()
	_, err := ImportBank(ImportOptions{From: from, All: true, DryRun: true})
	var in *ImportInputError
	if !errors.As(err, &in) || !strings.Contains(err.Error(), "retired per-repo layout") {
		t.Errorf("import of a per-repo source into a flat store err = %v, want the migrate hint", err)
	}
	if _, err := os.Stat(filepath.Join(StoreDir(), layoutFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("import --dry-run stamped the store layout (stat err = %v); it must write nothing", err)
	}
	write(t, filepath.Join(StoreDir(), "alpha", "progress.md"), "x\n")
	write(t, filepath.Join(from, layoutFile), layoutFlat+"\n")
	if _, err := ImportBank(ImportOptions{From: from, All: true, DryRun: true}); !errors.Is(err, errStoreNotFlat) {
		t.Errorf("import into a per-repo store err = %v, want errStoreNotFlat", err)
	}
}

func flatImportFixture(t *testing.T) (string, Bank) {
	t.Helper()
	isolatedConfig(t)
	initStore(t)
	b := Bank{Key: "alpha"}
	write(t, b.Path("decisionLog.md"), "# Log\n\n## 2026-11-01 — local <!-- id:11111111 -->\nLocal body.\n")
	write(t, b.NativeIndex(), "- [Local](alpha--local.md)\n")
	write(t, b.Path("topic.md"), "keep local\n")
	src := t.TempDir()
	files := map[string]string{
		layoutFile: layoutFlat + "\n", "alpha--decisionLog.md": "# Log\n\n## 2026-11-02 — remote <!-- id:22222222 -->\nRemote body.\n",
		"alpha--topic.md": "remote conflict\n", "alpha--new.md": "new topic\n", "MEMORY.md": "- [Remote](alpha--new.md)\n- [Orphan](orphan--new.md)\n",
		"orphan--decisionLog.md": "## 2026-11-02 — orphan <!-- id:33333333 -->\n", "orphan--new.md": "orphan topic\n",
		catalogFile: "foreign catalog\n", "alpha--writer.lock": "lock\n", ".git/config": "foreign git\n",
		"archive/alpha--decisionLog-2026-10.md": "## 2026-10-01 — archived <!-- id:44444444 -->\n",
		"native-only.md":                        "shared topic\n", "reports/alpha--bundle/report.md": "nested report\n", "locks/alpha--writer.md": "lock metadata\n",
	}
	for rel, body := range files {
		write(t, filepath.Join(src, rel), body)
	}
	write(t, filepath.Join(src, "alpha--huge.md"), strings.Repeat("x", importMaxFile+1))
	if err := os.Symlink("alpha--new.md", filepath.Join(src, "alpha--link.md")); err != nil {
		t.Fatal(err)
	}
	return src, b
}

func TestImportFlatAdditiveAndIdempotent(t *testing.T) {
	src, b := flatImportFixture(t)
	o := ImportOptions{From: src, All: true, Topics: true}
	res, err := ImportBank(o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Summary, "1 conflict(s)") || !strings.Contains(strings.Join(res.Lines, "\n"), "skipped orphan: no local store") {
		t.Fatalf("%+v", res)
	}
	log := aliasRead(t, b.Path("decisionLog.md"))
	if !strings.Contains(log, "11111111") || !strings.Contains(log, "22222222") {
		t.Fatal(log)
	}
	wantFile(t, b.Path("topic.md"), "keep local\n")
	wantFile(t, b.Path("new.md"), "new topic\n")
	wantFile(t, filepath.Join(StoreDir(), "native-only.md"), "shared topic\n")
	wantFile(t, b.Sub("reports", "bundle/report.md"), "nested report\n")
	if strings.Contains(aliasRead(t, b.NativeIndex()), "orphan--new.md") {
		t.Fatal("imported pointer for absent bank")
	}
	if !strings.Contains(aliasRead(t, b.NativeIndex()), "alpha--new.md") {
		t.Fatal("missing shared pointer")
	}
	for _, rel := range []string{"orphan--decisionLog.md", "orphan--new.md", "alpha--writer.lock", "alpha--huge.md", "alpha--link.md", "locks/alpha--writer.md"} {
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
	src, b := flatImportFixture(t)
	o := ImportOptions{From: src, Repo: b.Key, DryRun: true}
	res, err := ImportBank(o)
	if err != nil || !strings.Contains(res.Summary, "would import") {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Contains(aliasRead(t, b.Path("decisionLog.md")), "22222222") {
		t.Fatal("dry run wrote")
	}
	o.DryRun = false
	if _, err := ImportBank(o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(aliasRead(t, b.Path("decisionLog.md")), "22222222") {
		t.Fatal("missing remote log")
	}
	if pathExists(b.Path("new.md")) || strings.Contains(aliasRead(t, b.NativeIndex()), "alpha--new.md") {
		t.Fatal("topics imported without flag")
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

func TestSourceStoresCutsKeyAtFirstDoubleDash(t *testing.T) {
	from := t.TempDir()
	for _, n := range []string{"my-repo--progress.md", "my--repo--progress.md", "progress.md"} {
		write(t, filepath.Join(from, n), "x\n")
	}
	stores, err := sourceStores(from)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stores["my-repo"]; !ok || len(stores) != 2 {
		t.Errorf("stores = %v, want only home and my-repo (a first-cut key \"my\" has no typed file)", stores)
	}
}
