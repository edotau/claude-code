package memory

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
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
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Helper()
	isolatedConfig(t)
	initStore(t)
	write(t, Path("decisionLog.md"), "# Log\n\n## 2026-11-01 — local <!-- id:11111111 -->\nLocal body.\n")
	write(t, NativeIndex(), "- [Local](local.md)\n")
	write(t, Path("topic.md"), "keep local\n")
	src := t.TempDir()
	files := map[string]string{
		layoutFile: layoutFlat + "\n", "decisionLog.md": "# Log\n\n## 2026-11-02 — remote <!-- id:22222222 -->\nRemote body.\n",
		"topic.md": "remote conflict\n", "new.md": "new topic\n", "MEMORY.md": "## Old header\n- [Remote](new.md) — remote hook\n",
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
	wantFile(t, Sub("notes", "topic.md"), "keep local\n")
	wantFile(t, Sub("notes", "new.md"), "new topic\n")
	wantFile(t, Sub("notes", "native-only.md"), "shared topic\n")
	wantFile(t, Sub("reports", "bundle/report.md"), "nested report\n")
	if idx := aliasRead(t, NativeIndex()); !strings.Contains(idx, "- [Remote](new.md) — remote hook\n") || !strings.Contains(idx, "- [Local](local.md)\n") ||
		!slices.Contains(stored(t), "## Old header") {
		t.Fatalf("MEMORY.md = %q, want the remote line verbatim and the local line kept", idx)
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
	if pathExists(Sub("notes", "new.md")) || strings.Contains(aliasRead(t, NativeIndex()), "(new.md)") {
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
	_, err := ExtractStoreTar(importTar(t, map[string]string{layoutFile: layoutFlat + "\n", "alpha--decisionLog.md": "entry\n", nativeIndexFile: "pointer\n", catalogFile: "catalog\n", routerFile: "router\n", "alpha--writer.lock": "lock\n", ".git/config": "git\n"}), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{layoutFile, "alpha--decisionLog.md", nativeIndexFile, catalogFile} {
		if !pathExists(filepath.Join(dir, rel)) {
			t.Errorf("missing %s from host archive", rel)
		}
	}
	for _, rel := range []string{routerFile, "alpha--writer.lock", ".git/config"} {
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

// pointerOnlySource is a flat source whose files all match the destination's, but whose MEMORY.md has a pointer line.
func pointerOnlySource(t *testing.T, line string) string {
	t.Helper()
	const note = "---\nname: a\ndescription: d\n---\n"
	write(t, Sub("notes", "a.md"), note)
	src := t.TempDir()
	write(t, filepath.Join(src, layoutFile), layoutFlat+"\n")
	write(t, filepath.Join(src, "notes", "a.md"), note)
	write(t, filepath.Join(src, "MEMORY.md"), line+"\n")
	return src
}

func TestImportOfOnlyPointerLinesMergesAndRegenerates(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	line := "- [a](notes/a.md) — remote hook wording"
	res, err := ImportBank(ImportOptions{From: pointerOnlySource(t, line), Topics: true})
	if err != nil || !strings.Contains(res.Summary, "1 pointer line(s)") {
		t.Fatalf("%+v, %v", res, err)
	}
	if !strings.Contains(aliasRead(t, NativeIndex()), line+"\n") || !slices.Contains(stored(t), line) {
		t.Error("the remote line must reach MEMORY.md and the catalog")
	}
	if res, err = ImportBank(ImportOptions{From: pointerOnlySource(t, line), Topics: true}); err != nil || res.Summary != "nothing to import" {
		t.Errorf("repeat: %+v, %v", res, err)
	}
}

func TestImportReadsOnlyTheRecordsOfTheSourceIndex(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	line, free := "- [a](notes/a.md) — remote hook wording", "remote free text\n\n  second paragraph"
	src := pointerOnlySource(t, line)
	write(t, filepath.Join(src, catalogFile), "# Memory catalog\n\n- [x](notes/x.md) — router row\n\n"+encodeRecord(free))
	res, err := ImportBank(ImportOptions{From: src, Topics: true})
	if err != nil || !strings.Contains(res.Summary, "2 pointer line(s)") {
		t.Fatalf("%+v, %v; want the stored record and the pending pointer", res, err)
	}
	if got := stored(t); !slices.Equal(got, []string{free, line}) {
		t.Errorf("index.md = %q, want the source's record, then its pending line, and no router row", got)
	}
}

func TestImportOfAnOldFormatStoreKeepsEveryEntryShape(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	src := pointerOnlySource(t, "")
	entries := []string{"- [odd](notes/odd.md) - first\n  wrapped continuation for odd", "* [star](notes/star.md) - star",
		"[bare](notes/bare.md) - bare"}
	for _, n := range []string{"odd", "star", "bare"} {
		note := "---\nname: " + n + "\ndescription: d\n---\n"
		write(t, Sub("notes", n+".md"), note)
		write(t, filepath.Join(src, "notes", n+".md"), note)
	}
	write(t, filepath.Join(src, catalogFile), "# Memory catalog\n\n## Index\n\n- decisionLog.md (1 entries)\n")
	write(t, filepath.Join(src, "MEMORY.md"), strings.Join(entries, "\n")+"\n")
	if _, err := ImportBank(ImportOptions{From: src, Topics: true}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !slices.Contains(stored(t), e) {
			t.Errorf("entry %q did not reach the catalog whole", e)
		}
	}
}

func TestImportPinOverflowFailsButStoresThePointers(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	seedNotes(t, 230, seq(230)...)
	line := "- [a](notes/a.md) — remote hook wording"
	_, err := ImportBank(ImportOptions{From: pointerOnlySource(t, line), Topics: true})
	if !errors.Is(err, errIndexProblem) {
		t.Fatalf("err = %v, want the pin overflow", err)
	}
	if !slices.Contains(stored(t), line) {
		t.Error("the merged pointer line must be durable in index.md")
	}
}

// replaceFixture is a store holding topic.md as "local\n" and a source whose topic.md differs.
func replaceFixture(t *testing.T) string {
	t.Helper()
	isolatedConfig(t)
	initStore(t)
	write(t, Path("topic.md"), "local\n")
	if _, _, err := StoreRepo().Commit([]string{"topic.md"}, "seed", ""); err != nil { // a bundle needs a commit
		t.Fatal(err)
	}
	src := t.TempDir()
	write(t, filepath.Join(src, layoutFile), layoutFlat+"\n")
	write(t, filepath.Join(src, "topic.md"), "remote\n")
	return src
}

func TestImportReplaceNeedsMatchingHash(t *testing.T) {
	src := replaceFixture(t)
	o := ImportOptions{From: src, Topics: true}
	if res, err := ImportBank(o); err != nil || !slices.Contains(res.Lines, "conflict topic.md: kept local") {
		t.Fatalf("no Replace: %v, %v; want a conflict", res.Lines, err)
	}
	o.Replace = map[string]string{"topic.md": sha256Hex([]byte("stale\n"))}
	if res, err := ImportBank(o); err != nil || !slices.Contains(res.Lines, "conflict topic.md: kept local") {
		t.Fatalf("wrong hash: %v, %v; want a conflict", res.Lines, err)
	}
	wantFile(t, Path("topic.md"), "local\n")
	o.Replace = map[string]string{"topic.md": sha256Hex([]byte("local\n"))}
	res, err := ImportBank(o)
	if err != nil || !slices.Contains(res.Lines, "replace topic.md") || !strings.Contains(res.Summary, "1 copied, 0 kept local, 0 conflict(s)") {
		t.Fatalf("matching hash: %v %q, %v; want a replace counted as copied", res.Lines, res.Summary, err)
	}
	wantFile(t, Sub("notes", "topic.md"), "remote\n") // the settle step files the loose note
}

func TestWriteStoreTarRoundTrip(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	write(t, filepath.Join(StoreDir(), layoutFile), layoutFlat+"\n")
	write(t, Path("topic.md"), "topic\n")
	write(t, Path("notes/deep.md"), "deep\n")
	write(t, Path("writer.lock"), "lock\n")
	write(t, Path("locks/x.md"), "lock metadata\n")
	write(t, Path(".git/hook.md"), "git\n")
	rels := []string{"topic.md", "notes/deep.md", "writer.lock", "locks/x.md", ".git/config", ".git/hook.md", "../escape.md", "nope.md"}
	if err := os.Symlink("topic.md", Path("link.md")); err == nil {
		rels = append(rels, "link.md")
	}
	var buf bytes.Buffer
	if err := WriteStoreTar(&buf, rels); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if skipped, err := ExtractStoreTar(&buf, dst); err != nil || len(skipped) != 0 {
		t.Fatalf("extract: skipped %v, err %v", skipped, err)
	}
	var got []string
	_ = filepath.WalkDir(dst, func(p string, d fs.DirEntry, _ error) error {
		if rel, _ := filepath.Rel(dst, p); !d.IsDir() {
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(got)
	if want := []string{layoutFile, "notes/deep.md", "topic.md"}; !slices.Equal(got, want) {
		t.Fatalf("round trip carried %v, want %v", got, want)
	}
	wantFile(t, filepath.Join(dst, "notes", "deep.md"), "deep\n")
}

func bundleNames(t *testing.T) []string {
	t.Helper()
	names, _ := filepath.Glob(filepath.Join(paths.StateDir(), "backups", "memory-import-*.bundle"))
	return names
}

func TestImportBundleOncePerDayKeepsThree(t *testing.T) {
	src := replaceFixture(t)
	day := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	step := func(name string, at time.Time) {
		t.Helper()
		write(t, filepath.Join(src, name), name+"\n")
		if _, err := ImportBank(ImportOptions{From: src, Topics: true, Now: at}); err != nil {
			t.Fatal(err)
		}
	}
	step("a.md", day)
	step("b.md", day.Add(time.Hour))      // inside 24 h: no second bundle
	if n := len(bundleNames(t)); n != 1 { // the b.md import is inside 24 h of a.md's
		t.Fatalf("%d bundles after two imports in one day, want 1", n)
	}
	for i := 1; i <= 4; i++ {
		step(fmt.Sprintf("c%d.md", i), day.Add(time.Duration(i)*25*time.Hour))
	}
	names := bundleNames(t)
	if len(names) != 3 || !strings.Contains(names[2], "20261105") {
		t.Fatalf("bundles %v, want the newest 3 ending 20261105", names)
	}
}

func TestImportAbortsWhenBundleFails(t *testing.T) {
	src := replaceFixture(t)
	write(t, filepath.Join(paths.StateDir(), "backups"), "a file where the folder belongs\n")
	write(t, filepath.Join(src, "new.md"), "new\n")
	if _, err := ImportBank(ImportOptions{From: src, Topics: true}); err == nil || !strings.Contains(err.Error(), "nothing was imported") {
		t.Fatalf("err = %v, want the backup refusal", err)
	}
	if pathExists(Path("new.md")) {
		t.Fatal("new.md was written although the backup failed")
	}
}

func TestImportWaitsForJobLockThenBusy(t *testing.T) {
	src := replaceFixture(t)
	t.Setenv(DetachedEnv, "")
	old := updateLockWait
	updateLockWait = 50 * time.Millisecond
	t.Cleanup(func() { updateLockWait = old })
	ran, err := paths.TryFileLock(lockPathOf()+".lock", time.Second, func() error {
		_, ierr := ImportBank(ImportOptions{From: src, Topics: true})
		return ierr
	})
	if !ran || !errors.Is(err, ErrBankBusy) {
		t.Fatalf("ran=%v err=%v, want ErrBankBusy under a held job lock", ran, err)
	}
}
