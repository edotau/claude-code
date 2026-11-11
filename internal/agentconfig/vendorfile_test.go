package agentconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// A failed later write must restore every file the plan already changed (hooks.json + config.toml pairs).
func TestApplyRollsBackOnFailure(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "hooks.json")
	created := filepath.Join(dir, "new.json")
	if err := os.WriteFile(existing, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	p := &RenderPlan{writes: []stagedWrite{
		{path: existing, write: func() error { return os.WriteFile(existing, []byte("new"), 0o600) }},
		{path: created, write: func() error { return os.WriteFile(created, []byte("x"), 0o600) }},
		{path: filepath.Join(dir, "config.toml"), write: func() error { return boom }},
	}}
	if err := p.Apply(); !errors.Is(err, boom) {
		t.Fatalf("Apply = %v, want boom", err)
	}
	if got, _ := os.ReadFile(existing); string(got) != "old" {
		t.Fatalf("hooks.json = %q, want restored %q", got, "old")
	}
	if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new.json should be removed after rollback, stat err = %v", err)
	}
}

// backupEnv isolates HOME and CLAUDE_CONFIG_DIR and returns a vendor config path under HOME.
func backupEnv(t *testing.T) (path, mirrorGlob string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	path = filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	mirrorGlob = filepath.Join(paths.StateDir(), "backups", ".gemini", "antigravity-cli", "settings.json.bak-*")
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
		if err := backupAndWrite(path, old, fmt.Appendf(nil, "v%d", i), existed); err != nil {
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

func TestCommitJSONObject(t *testing.T) {
	cases := []struct {
		name, content     string
		missing, noMutate bool
		unreadable        bool
		wantFile          string
		wantNote          string
		wantChanged       bool
	}{
		{name: "missing", missing: true, wantFile: "{\n  \"k\": \"v\"\n}\n", wantChanged: true},
		{name: "object keeps other keys", content: `{"a":1}`, wantFile: "{\n  \"a\": 1,\n  \"k\": \"v\"\n}\n", wantChanged: true},
		{name: "null", content: "null", wantFile: "null", wantNote: "not an object"},
		{name: "array", content: "[1]", wantFile: "[1]", wantNote: "not plain JSON"},
		{name: "bad json", content: "{bad", wantFile: "{bad", wantNote: "not plain JSON"},
		{name: "trailing close brace", content: `{"a":1}}`, wantFile: `{"a":1}}`, wantNote: "trailing data"},
		{name: "trailing bracket", content: `{"a":1} ]`, wantFile: `{"a":1} ]`, wantNote: "trailing data"},
		{name: "blank existing file", content: "", wantFile: "{\n  \"k\": \"v\"\n}\n", wantChanged: true},
		{name: "whitespace existing file", content: "  \n", wantFile: "{\n  \"k\": \"v\"\n}\n", wantChanged: true},
		{name: "unreadable", content: `{"a":1}`, unreadable: true, wantFile: `{"a":1}`, wantNote: "cannot read"},
		{name: "mutate false", content: `{"a":1}`, noMutate: true, wantFile: `{"a":1}`},
		{name: "mutate false on missing writes nothing", missing: true, noMutate: true, wantFile: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(os.Getenv("HOME"), ".claude"))
			path := filepath.Join(t.TempDir(), "settings.json")
			if !c.missing {
				mode := os.FileMode(0o600)
				if c.unreadable {
					mode = 0
				}
				if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			var plan RenderPlan
			changed, err := CommitJSONObject(path, RenderOptions{}.Stage(&plan), &out, "x-settings", func(obj map[string]any, _ bool) bool {
				obj["k"] = "v"
				return !c.noMutate
			})
			if err == nil {
				err = plan.Apply()
			}
			if err != nil {
				t.Fatal(err)
			}
			if changed != c.wantChanged {
				t.Errorf("changed = %v, want %v", changed, c.wantChanged)
			}
			os.Chmod(path, 0o600)
			got, _ := os.ReadFile(path)
			if string(got) != c.wantFile {
				t.Errorf("file = %q, want %q", got, c.wantFile)
			}
			if want := "x-settings: skipped: "; c.wantNote != "" && !strings.Contains(out.String(), want) ||
				!strings.Contains(out.String(), c.wantNote) {
				t.Errorf("note = %q, want %q + %q", out.String(), want, c.wantNote)
			}
		})
	}
}

func TestRenderHooksFileRejectsNullExistingObject(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "hooks.json")
	const existing = "null\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	merge := func(raw, body []byte) ([]byte, error) {
		next, _, err := MergeHooksTop(raw, body, func(item json.RawMessage) (*json.RawMessage, bool) {
			return &item, false
		})
		if err != nil {
			return nil, err
		}
		return json.Marshal(next)
	}
	var plan RenderPlan
	changed, err := RenderHooksFile(path, []byte(`{"hooks":{"SessionStart":[{"command":"claude-code hook SessionStart"}]}}`), merge, RenderOptions{}.Stage(&plan), io.Discard, "codex-hooks")
	if err == nil {
		t.Fatal("existing null hooks file accepted; want non-object error")
	}
	if !strings.Contains(err.Error(), "existing file: not a JSON object") {
		t.Fatalf("error = %v, want existing-file non-object error", err)
	}
	if changed {
		t.Fatal("rejected hooks file reported changed")
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != existing {
		t.Fatalf("hooks file = %q, want original %q", got, existing)
	}
}
