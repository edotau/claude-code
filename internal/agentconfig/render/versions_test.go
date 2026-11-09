package render

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// backupEnv isolates HOME and CLAUDE_CONFIG_DIR and returns a vendor config path under HOME.
func backupEnv(t *testing.T) (path, mirrorGlob string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	path = filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	mirrorGlob = filepath.Join(paths.CacheDir(), "backups", ".gemini", "antigravity-cli", "settings.json.bak-*")
	return path, mirrorGlob
}

func writeVersions(t *testing.T, path string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		existed := i > 1
		var old []byte
		if existed {
			old, _ = os.ReadFile(path)
		}
		if err := backupAndWrite(path, old, []byte(fmt.Sprintf("v%d", i)), existed); err != nil {
			t.Fatal(err)
		}
	}
}

// No <path>.backup* sibling is left, and the previous content lands in the paths mirror.
func TestBackupLeavesNoSiblingAndMirrorsPrevious(t *testing.T) {
	path, glob := backupEnv(t)
	writeVersions(t, path, 2)
	if sib, _ := filepath.Glob(path + ".backup*"); len(sib) != 0 {
		t.Fatalf("sibling backup files left next to config: %v", sib)
	}
	mirror, _ := filepath.Glob(glob)
	if len(mirror) != 1 {
		t.Fatalf("want 1 mirror copy of v1 under %s, got %v", glob, mirror)
	}
	if got, _ := os.ReadFile(mirror[0]); string(got) != "v1" {
		t.Fatalf("mirror content = %q, want v1", got)
	}
}

// Seven writes keep at most 5 mirror copies, and they are the newest five previous contents.
func TestMirrorPrunedToFive(t *testing.T) {
	path, glob := backupEnv(t)
	writeVersions(t, path, 7)
	mirror, _ := filepath.Glob(glob)
	if len(mirror) != 5 {
		t.Fatalf("want 5 mirror copies after 7 writes, got %d: %v", len(mirror), mirror)
	}
	sort.Strings(mirror)
	var got []string
	for _, m := range mirror {
		b, _ := os.ReadFile(m)
		got = append(got, string(b))
	}
	if want := "v2,v3,v4,v5,v6"; strings.Join(got, ",") != want {
		t.Fatalf("kept mirror contents = %v, want %s", got, want)
	}
}

// Revert restores the newest mirror copy's content (the content before the last write).
func TestRevertRestoresNewestMirror(t *testing.T) {
	path, _ := backupEnv(t)
	writeVersions(t, path, 4)
	if err := RevertOne("gemini-settings", path, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "v3" {
		t.Fatalf("after revert content = %q, want v3", got)
	}
}

// TestRevertLegacySidecarWhenMirrorEmpty pins: with no mirror backup, revert restores from a legacy <path>.backup.<stamp>.
func TestRevertLegacySidecarWhenMirrorEmpty(t *testing.T) {
	path, _ := backupEnv(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".backup.20260101T000000", []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RevertOne("gemini-settings", path, false, &out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "old" {
		t.Fatalf("config = %q, want %q (output: %s)", got, "old", out.String())
	}
}

func TestRevertLegacySidecarsStepBack(t *testing.T) {
	path, _ := backupEnv(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	for suffix, content := range map[string]string{
		"":                        "v3",
		".backup.20260101T000000": "v1",
		".backup.20260102T000000": "v2",
	} {
		if err := os.WriteFile(path+suffix, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if mirrors := mirrorsOf(path); len(mirrors) != 0 {
		t.Fatalf("want no mirrored backups, got %v", mirrors)
	}
	for _, want := range []string{"v2", "v1"} {
		if err := RevertOne("gemini-settings", path, false, io.Discard); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("successive legacy revert = %q, want %q", got, want)
		}
	}
}

// TestLostLinesEmptyWhenTheirsMatchesBaseline: theirs (current file) equal to the last-written baseline
// means the user made no local edits — no lost lines, even when the upcoming render (next) differs.
func TestLostLinesEmptyWhenTheirsMatchesBaseline(t *testing.T) {
	base := []byte("a\nb\nc\n")
	theirs := base // no hand edit since the last render
	next := []byte("a\nb\nc\nd\n")
	if got := lostLines(theirs, base, next); got != nil {
		t.Errorf("lostLines(theirs==base) = %v, want nil", got)
	}
}

// TestLostLinesDetectsHandAddedLine: a line present in theirs but absent from both base and next is the
// user's own edit, lost by the upcoming render.
func TestLostLinesDetectsHandAddedLine(t *testing.T) {
	base := []byte("a\nb\n")
	theirs := []byte("a\nb\nuser-added\n")
	next := []byte("a\nb\n")
	got := lostLines(theirs, base, next)
	if len(got) != 1 || got[0] != "user-added" {
		t.Errorf("lostLines = %v, want [\"user-added\"]", got)
	}
}

// TestVersionFilesCappedAtFive: backupAndWrite keeps the newest 5 cache-mirrored backups, oldest dropped first.
func TestVersionFilesCappedAtFive(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 7; i++ {
		old, _ := os.ReadFile(path)
		if err := backupAndWrite(path, old, []byte(fmt.Sprintf("v%d", i)), true); err != nil {
			t.Fatal(err)
		}
	}
	m := mirrorsOf(path)
	if len(m) != 5 {
		t.Fatalf("mirrors = %d, want 5: %v", len(m), m)
	}
	if got, _ := os.ReadFile(m[0]); string(got) != "v6" {
		t.Errorf("newest mirror = %q, want v6", got)
	}
	if got, _ := os.ReadFile(m[4]); string(got) != "v2" {
		t.Errorf("oldest kept mirror = %q, want v2", got)
	}
}

// TestLostLinesForSurfacesUnreadableBaseline: a baseline that exists but cannot be read (here a directory)
// is an error, not "no baseline"; only a missing one skips hand-edit protection.
func TestLostLinesForSurfacesUnreadableBaseline(t *testing.T) {
	path, _ := backupEnv(t)
	const label = "unit-no-ops"
	if got, err := lostLinesFor(label, path, []byte("a\n"), []byte("b\n")); got != nil || err != nil {
		t.Fatalf("missing baseline = %v, %v; want nil, nil", got, err)
	}
	if err := os.MkdirAll(baselinePath(label, path), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := lostLinesFor(label, path, []byte("a\n"), []byte("b\n")); err == nil {
		t.Fatal("unreadable baseline returned nil error, want the read error")
	}
}
