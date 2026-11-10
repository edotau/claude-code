package docs

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
)

func scanWinPaths(text string) []string {
	var out []string
	for _, tok := range scanWinTokens(text) {
		out = append(out, tok.trimmed)
	}
	return out
}

func TestDescribeRejectsSpecialFiles(t *testing.T) {
	if _, _, ok := describe(os.DevNull); ok {
		t.Fatal("null device reported as a regular attachment")
	}
}

// promptPathFeedback runs the hook core with res injected, so tests do not depend on the host's mounts.
func promptPathFeedback(r io.Reader, stdout io.Writer, res *paths.Resolver) int {
	in := core.ParseInput(r)
	return core.Emit(in.Agent(), "UserPromptSubmit", pathResolverResult(in, res), stdout, io.Discard)
}

func TestScanWinPaths(t *testing.T) {
	// Quoted-with-spaces must be captured whole (not split by the bareword regex).
	got := scanWinPaths(`look at C:\Users\x\a.png and "C:\Users\x b\c.png" plus D:/tmp/d.png`)
	want := map[string]bool{
		`C:\Users\x b\c.png`: true, // quoted first
		`C:\Users\x\a.png`:   true,
		`D:/tmp/d.png`:       true,
	}
	if len(got) != len(want) {
		t.Fatalf("scanWinPaths returned %d paths, want %d: %v", len(got), len(want), got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected path %q", p)
		}
	}
}

// The user gets NO terminal feedback that a dropped path was recognized — the additive context is
// model-visible only. A resolved path must also come back as a systemMessage, and an image must be
// described (type, dimensions, size) since a terminal cannot preview it.
func TestPathResolverEmitsUserVisibleFeedback(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "shot.png")
	writePNG(t, img, 640, 480)

	var out bytes.Buffer
	in := strings.NewReader(`{"prompt":"look at ` + img + `"}`)
	if code := promptPathFeedback(in, &out, paths.NewResolver(nil)); code != core.ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed", code)
	}
	var got struct {
		SystemMessage      string `json:"systemMessage"`
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid hook JSON: %v\n%s", err, out.String())
	}
	if got.SystemMessage == "" {
		t.Fatal("no systemMessage — the user still gets no feedback")
	}
	for _, want := range []string{"shot.png", "640", "480", "PNG"} {
		if !strings.Contains(got.SystemMessage, want) {
			t.Errorf("systemMessage missing %q:\n%s", want, got.SystemMessage)
		}
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, img) {
		t.Errorf("additionalContext missing the image path:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

// Off WSL with no image and no Windows path, the hook stays silent — no stray banner on every prompt.
func TestPromptPathFeedbackSilentWhenNothingToReport(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader(`{"prompt":"just prose about a png in the abstract"}`)
	promptPathFeedback(in, &out, paths.NewResolver(nil))
	if out.Len() != 0 {
		t.Errorf("expected silence, got:\n%s", out.String())
	}
}

// A non-image file still earns a one-line confirmation that the path resolved.
func TestPromptPathFeedbackReportsNonImageFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(f, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	in := strings.NewReader(`{"prompt":"read ` + f + `"}`)
	promptPathFeedback(in, &out, paths.NewResolver(nil))
	if !strings.Contains(out.String(), "notes.md") {
		t.Errorf("expected the resolved path in the feedback:\n%s", out.String())
	}
}

// writePNG writes a real PNG of the given size so DecodeConfig reads true dimensions.
func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
}

// A POSIX path inside a foreign token (~/etc/hostname) must not be reported as /etc/hostname.
func TestPromptPathsIgnorePosixInsideForeignToken(t *testing.T) {
	if _, err := os.Stat("/etc/hostname"); err != nil {
		t.Skip("needs /etc/hostname")
	}
	res := &paths.Resolver{Home: t.TempDir()}
	for _, prompt := range []string{"read ~/etc/hostname", `read "~/etc/hostname"`, "read file://nohost/etc/hostname"} {
		if reps := collectReports(prompt, res); len(reps) != 0 {
			t.Errorf("%q leaked %+v", prompt, reps)
		}
	}
}

// host:/path must be recognized whole, not read as the drive path "s:/path".
func TestPromptPathsHostPathNotDrive(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "d", "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := &paths.Resolver{Mounts: []paths.Mount{
		{Point: dir, Kind: paths.KindNFS, Host: "nas", RemotePath: "/export/data"}}}
	var out bytes.Buffer
	promptPathFeedback(strings.NewReader(`{"prompt":"open nas:/export/data/d/a.txt now"}`), &out, res)
	if !strings.Contains(out.String(), f) {
		t.Errorf("host:/path not resolved to %s:\n%s", f, out.String())
	}
}

// Prose punctuation after an unquoted foreign token is not part of the path.
func TestScanWinPathsTrimsTrailingPunctuation(t *testing.T) {
	got := scanWinPaths("see file:///etc/hostname. and (C:\\a\\b.txt), ok")
	want := []string{"file:///etc/hostname", `C:\a\b.txt`}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("scanWinPaths = %q, want %q", got, want)
	}
}

// A markdown backtick closing an inline-code path is not part of the path.
func TestScanWinPathsStopsAtBacktick(t *testing.T) {
	got := scanWinPaths("read `file:///etc/hostname` now")
	if len(got) != 1 || got[0] != "file:///etc/hostname" {
		t.Errorf("scanWinPaths = %q, want [file:///etc/hostname]", got)
	}
}

// A trailing "!" or ")" belongs to the filename only when that file exists; a plain sibling is reported separately.
func TestCollectReportsPunctuatedFilenames(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		text  func(d string) string
		want  []string
	}{
		{"bang name", []string{"report (1)", "wow!"}, func(d string) string { return "read " + d + "/wow! now" }, []string{"wow!"}},
		{"paren wrapped", []string{"report (1)", "wow!"}, func(d string) string { return "see (" + d + "/wow!)" }, []string{"wow!"}},
		{"both siblings", []string{"wow!", "wow"}, func(d string) string { return d + "/wow! " + d + "/wow" }, []string{"wow!", "wow"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range c.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			reps := collectReports(c.text(dir), &paths.Resolver{})
			if len(reps) != len(c.want) {
				t.Fatalf("reps = %+v, want %v", reps, c.want)
			}
			for i, w := range c.want {
				if filepath.Base(reps[i].path) != w {
					t.Errorf("reps[%d] = %q, want %q", i, reps[i].path, w)
				}
			}
		})
	}
}

func stubBridge(t *testing.T, ready bool, fetch func(string) (string, error)) {
	t.Helper()
	oReady, oFetch := bridgeReady, fetchClient
	bridgeReady = func() bool { return ready }
	fetchClient = fetch
	t.Cleanup(func() { bridgeReady, fetchClient = oReady, oFetch })
}

// A /Users path that exists only on the SSH client is fetched over the bridge in every form a Mac Terminal drops it.
func TestCollectReportsFetchesClientOnlyPath(t *testing.T) {
	const mac = "/Users/me/Downloads/My File.zip"
	local := filepath.Join(t.TempDir(), "My File.zip")
	if err := os.WriteFile(local, []byte("zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	forms := map[string]string{
		"double quoted": `see "` + mac + `" now`,
		"single quoted": `see '` + mac + `' now`,
		"escaped space": `see /Users/me/Downloads/My\ File.zip now`,
	}
	for name, prompt := range forms {
		t.Run(name, func(t *testing.T) {
			var asked []string
			stubBridge(t, true, func(p string) (string, error) { asked = append(asked, p); return local, nil })
			reps := collectReports(prompt, &paths.Resolver{})
			if len(reps) != 1 || reps[0].win != mac || reps[0].path != local || reps[0].err != "" {
				t.Fatalf("reps = %+v, want one %s -> %s", reps, mac, local)
			}
			if len(asked) != 1 || asked[0] != mac {
				t.Errorf("fetched %v, want [%s]", asked, mac)
			}
			if got := modelContext(reps); !strings.Contains(got, mac) || !strings.Contains(got, local) {
				t.Errorf("modelContext = %q, want both paths", got)
			}
		})
	}
}

func TestCollectReportsClientFetchFailureAndGates(t *testing.T) {
	failing := func(string) (string, error) { return "", os.ErrPermission }
	stubBridge(t, true, failing)
	reps := collectReports(`open /Users/me/a.png`, &paths.Resolver{})
	if len(reps) != 1 || !strings.Contains(reps[0].err, "permission") {
		t.Fatalf("reps = %+v, want one error report", reps)
	}
	if msg := feedbackMessage(reps); !strings.Contains(msg, "not fetched") || !strings.Contains(msg, "permission") {
		t.Errorf("feedback = %q, want a not-fetched line naming the error", msg)
	}
	calls := 0
	counting := func(string) (string, error) { calls++; return "", os.ErrPermission }
	stubBridge(t, false, counting)
	if reps := collectReports(`open /Users/me/a.png`, &paths.Resolver{}); len(reps) != 0 || calls != 0 {
		t.Errorf("bridge down: reps=%+v calls=%d, want none", reps, calls)
	}
	stubBridge(t, true, counting)
	if reps := collectReports(`open /home/nobody/a.png`, &paths.Resolver{}); len(reps) != 0 || calls != 0 {
		t.Errorf("non-Mac prefix: reps=%+v calls=%d, want none", reps, calls)
	}
}
