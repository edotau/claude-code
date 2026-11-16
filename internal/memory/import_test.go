package memory

import (
	"archive/tar"
	"bytes"
	"errors"
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

// importFixture seeds the local store and returns a source root extracted from a hostile tar.
func importFixture(t *testing.T, e aliasEnv) string {
	t.Helper()
	entry := func(id, day, body string) string {
		return "## " + day + " — T <!-- id:" + id + " -->\n" + body + "\n\n"
	}
	hdr := "# Decision Log\n\n"
	local := map[string]string{"decisionLog.md": hdr + entry("11111111", "2026-11-01", "local"), "activeContext.md": "local focus\n"}
	for name, body := range local {
		if err := os.WriteFile(filepath.Join(e.da, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := t.TempDir()
	skipped, err := ExtractStoreTar(importTar(t, map[string]string{
		e.ka + "/decisionLog.md":   hdr + entry("22222222", "2026-11-02", "remote") + entry("11111111", "2026-11-01", "local"),
		e.ka + "/activeContext.md": "remote focus\n", e.ka + "/new-topic.md": "topic\n",
		e.ka + "/link.md": "->/etc/passwd", "../escape.md": "x", "index.md": "catalog",
	}), src)
	if err != nil || len(skipped) != 2 || pathExists(filepath.Join(filepath.Dir(src), "escape.md")) {
		t.Fatalf("extract: skipped=%v err=%v", skipped, err)
	}
	return src
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

// importDryRunLogsOnly asserts the default dry run writes nothing and leaves the non-log files out of the plan.
func importDryRunLogsOnly(t *testing.T, src, repo, logPath string) {
	t.Helper()
	dry, err := ImportBank(ImportOptions{From: src, Repo: repo, DryRun: true})
	if err != nil || strings.Contains(aliasRead(t, logPath)+strings.Join(dry.Lines, "\n"), "remote focus") ||
		!strings.HasSuffix(dry.Summary, "(--topics imports them)") {
		t.Fatalf("default dry run (err=%v): %v", err, dry)
	}
	if strings.Contains(aliasRead(t, logPath), "remote") || strings.Contains(strings.Join(dry.Lines, "\n"), "new-topic") {
		t.Fatalf("dry run wrote, or planned a topic file: %v", dry)
	}
}

func TestImportBankMergesOnceAndNeverOverwrites(t *testing.T) {
	e := newAliasEnv(t)
	src := importFixture(t, e)
	logPath := filepath.Join(e.da, "decisionLog.md")
	importDryRunLogsOnly(t, src, e.a, logPath)
	opt := ImportOptions{From: src, Repo: e.a, Label: "test", Topics: true}
	res, err := ImportBank(opt)
	log := aliasRead(t, logPath)
	if err != nil || strings.Count(log, "remote") != 1 || strings.Count(log, "local") != 1 || !strings.HasPrefix(res.Summary, "imported") {
		t.Fatalf("import: %v\n%v\n%s", err, res, log)
	}
	for name, want := range map[string]string{"new-topic.md": "topic\n", "activeContext.md": "local focus\n", "link.md": ""} {
		wantFile(t, filepath.Join(e.da, name), want)
	}
	if again, err := ImportBank(opt); err != nil || again.Summary != "nothing to import" {
		t.Fatalf("second run: %v %v", err, again)
	}
}

// TestImportBankSkipsEntriesAlreadyArchived: an entry the local store rotated into its archive is not imported again.
func TestImportBankSkipsEntriesAlreadyArchived(t *testing.T) {
	e := newAliasEnv(t)
	const hdr, block = "# Decision Log\n\n", "## 2026-11-02 — T <!-- id:33333333 -->\nrotated\n\n"
	src := filepath.Join(t.TempDir(), e.ka)
	for path, body := range map[string]string{
		filepath.Join(e.da, "decisionLog.md"): hdr, filepath.Join(e.da, "archive", "decisionLog-2026-11.md"): hdr + block,
		filepath.Join(src, "decisionLog.md"): hdr + block,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := ImportBank(ImportOptions{From: src, Repo: e.a, DryRun: true})
	if err != nil || !strings.HasPrefix(res.Summary, "nothing to import") {
		t.Fatalf("an archived entry was planned again: %v %v", err, res)
	}
}

func TestImportRefusesSourceWithDifferentLayout(t *testing.T) {
	tempConfig(t)
	from := t.TempDir()
	write(t, filepath.Join(from, layoutFile), layoutFlat+"\n")
	_, err := ImportBank(ImportOptions{From: from, All: true, DryRun: true})
	var in *ImportInputError
	if !errors.As(err, &in) || !strings.Contains(err.Error(), "memory migrate") {
		t.Errorf("import of a flat source into a per-repo store err = %v, want the migrate hint", err)
	}
}

func flatImportFixture(t *testing.T) (string, Bank) {
	t.Helper()
	isolatedConfig(t)
	setStoreFlat(t, true)
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

type aliasEnv struct {
	cfg, a, b, ka, kb string // config dir, checkouts, store keys
	da, db            string // store dirs
}

// newAliasEnv builds an isolated config (itself a decoy git repo), a cut-over store and two repo stores.
func newAliasEnv(t *testing.T) aliasEnv {
	t.Helper()
	cfg, _ := isolatedConfig(t)
	gitRepo(t, filepath.Dir(cfg), filepath.Base(cfg))
	initStore(t)
	parent := canonTemp(t, t.TempDir())
	e := aliasEnv{cfg: cfg, a: gitRepo(t, parent, "alpha"), b: gitRepo(t, parent, "beta")}
	var err error
	if e.da, err = EnsureRepoDir(e.a); err != nil {
		t.Fatal(err)
	}
	if e.db, err = EnsureRepoDir(e.b); err != nil {
		t.Fatal(err)
	}
	e.ka, e.kb = filepath.Base(e.da), filepath.Base(e.db)
	return e
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

func TestFlatSourceStoresCutsKeyAtFirstDoubleDash(t *testing.T) {
	from := t.TempDir()
	for _, n := range []string{"my-repo--progress.md", "my--repo--progress.md", "progress.md"} {
		write(t, filepath.Join(from, n), "x\n")
	}
	stores, _, err := flatSourceStores(from)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stores["my-repo"]; !ok || len(stores) != 2 {
		t.Errorf("stores = %v, want only home and my-repo (a first-cut key \"my\" has no typed file)", stores)
	}
}
