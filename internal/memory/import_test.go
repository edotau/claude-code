package memory

import (
	"archive/tar"
	"bytes"
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
