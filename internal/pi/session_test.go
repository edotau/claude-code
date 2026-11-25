package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSession(t *testing.T, dir, name, cwd string) {
	t.Helper()
	hdr := `{"type":"session","cwd":"` + cwd + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(hdr), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionFileMatchesExactly(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "2026-11-25T19-20-37-171Z_ses-10.jsonl", "/x")
	writeSession(t, dir, "2026-11-25T19-20-37-171Z_xses-1.jsonl", "/x")
	for _, id := range []string{"ses-1", "ses", "", "*", "../ses-10", "ses-10.jsonl", "2026-11-25T19-20-37-171Z_ses-10"} {
		if got, err := sessionFile(dir, id); got != "" || err != nil {
			t.Errorf("sessionFile(%q) = %q, %v; want no match", id, got, err)
		}
	}
	if got, err := sessionFile(dir, "ses-10"); err != nil || filepath.Base(got) != "2026-11-25T19-20-37-171Z_ses-10.jsonl" {
		t.Errorf("sessionFile(ses-10) = %q, %v", got, err)
	}
}

func TestSessionFileTwoFilesIsError(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "2026-11-25T10-00-00-000Z_dup.jsonl", "/x")
	writeSession(t, dir, "2026-11-25T11-00-00-000Z_dup.jsonl", "/x")
	got, err := sessionFile(dir, "dup")
	if got != "" || err == nil || !strings.Contains(err.Error(), "10-00-00") || !strings.Contains(err.Error(), "11-00-00") {
		t.Fatalf("got %q, %v; want an error naming both files", got, err)
	}
}

func TestSessionCwd(t *testing.T) {
	dir, work := t.TempDir(), t.TempDir()
	writeSession(t, dir, "2026-11-25T10-00-00_ses-9.jsonl", work)
	if got, err := sessionCwd(dir, "ses-9"); err != nil || got != work {
		t.Fatalf("sessionCwd = %q, %v, want %q (Pi prompts to fork from any other directory)", got, err, work)
	}
	if got, err := sessionCwd(dir, "other"); got != "" || err != nil {
		t.Fatalf("sessionCwd of an unknown id = %q, %v, want empty", got, err)
	}
}

func resumeFixture(t *testing.T) (dir, work, other string) {
	t.Helper()
	dir, work, other = t.TempDir(), t.TempDir(), t.TempDir()
	writeSession(t, dir, "2026-11-25T10-00-00_ses-1.jsonl", work)
	writeSession(t, dir, "2026-11-25T10-00-00_ses-gone.jsonl", filepath.Join(other, "gone"))
	return dir, work, other
}

func TestResumeDirRuns(t *testing.T) {
	dir, work, other := resumeFixture(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(work, link); err != nil {
		t.Skip("no symlinks:", err)
	}
	for _, c := range []struct{ id, workDir, want string }{
		{"ses-1", "", work}, {"ses-1", work + "/", work}, {"ses-1", link, work}, {"", other, other}, {"unknown", other, other},
	} {
		if got, err := resumeDir(dir, c.id, c.workDir); err != nil || got != c.want {
			t.Errorf("resumeDir(%q, %q) = %q, %v; want %q", c.id, c.workDir, got, err, c.want)
		}
	}
}

func TestResumeDirRefuses(t *testing.T) {
	dir, work, other := resumeFixture(t)
	_, err := resumeDir(dir, "ses-1", other)
	if err == nil || !strings.Contains(err.Error(), work) || !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), "drop the workdir flag") {
		t.Errorf("a clash must name both directories, got %v", err)
	}
	_, err = resumeDir(dir, "ses-gone", "")
	if err == nil || !strings.Contains(err.Error(), filepath.Join(other, "gone")) {
		t.Errorf("a vanished session directory must be named, got %v", err)
	}
}

func TestEnsureSessionDirIsOwnerOnly(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(sessionDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureSessionDir(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(sessionDir()); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("session dir mode = %v, %v; want 0700", st, err)
	}
}
