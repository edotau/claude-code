package hooks

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/pathview"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
	if code := promptPathFeedback(in, &out, pathview.NewResolver(nil)); code != ExitProceed {
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
	promptPathFeedback(in, &out, pathview.NewResolver(nil))
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
	promptPathFeedback(in, &out, pathview.NewResolver(nil))
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

// TestDescribeProperly handles image files without losing close errors. Regression test for
// defect where file close errors in describe() were silently ignored, potentially indicating
// I/O issues that should be reported.
func TestDescribeProperly(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "test.png")
	writePNG(t, img, 100, 200)

	detail, isImage, ok := describe(img)

	if !ok {
		t.Fatal("describe failed to process valid image file")
	}
	if !isImage {
		t.Error("isImage = false, want true for PNG")
	}
	if !strings.Contains(detail, "100") || !strings.Contains(detail, "200") {
		t.Errorf("detail = %q, want dimensions included", detail)
	}
	if !strings.Contains(detail, "PNG") {
		t.Errorf("detail = %q, want PNG type included", detail)
	}
}

// A POSIX path inside a foreign token (~/etc/hostname) must not be reported as /etc/hostname.
func TestPromptPathsIgnorePosixInsideForeignToken(t *testing.T) {
	if _, err := os.Stat("/etc/hostname"); err != nil {
		t.Skip("needs /etc/hostname")
	}
	res := &pathview.Resolver{Home: t.TempDir()}
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
	res := &pathview.Resolver{Mounts: []pathview.Mount{
		{Point: dir, Kind: pathview.KindNFS, Host: "nas", RemotePath: "/export/data"}}}
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

// The paths and codex groups run natively: Bind must hand each declared handler the Runner from
// Native (same function pointer), not the legacy adapter, and that Runner returns a Result directly.
func TestBindPrefersNativeRunners(t *testing.T) {
	if len(Native) == 0 {
		t.Fatal("Native is empty")
	}
	seen := map[string]bool{}
	for _, b := range Bind() {
		native, ok := Native[b.Spec.Name]
		if !ok {
			continue
		}
		seen[b.Spec.Name] = true
		if reflect.ValueOf(b.Run).Pointer() != reflect.ValueOf(native).Pointer() {
			t.Errorf("%s: bound to the legacy adapter, want the Native runner", b.Spec.Name)
		}
	}
	for name := range Native {
		if !seen[name] {
			t.Errorf("%s: declared in Native but never bound", name)
		}
	}

	dir := t.TempDir()
	f := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(f, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := ParseInput(strings.NewReader(`{"prompt":"read ` + f + `"}`))
	for _, b := range Bind() {
		if b.Spec.Name != "path-resolver" {
			continue
		}
		r := b.Run(in) // native: a Result straight from the Runner, not re-parsed adapter output
		if r.Context == "" || r.SystemMessage == "" {
			t.Errorf("path-resolver Result = %+v, want Context and SystemMessage", r)
		}
	}
}

// A direct `hook path-resolver` call and the UserPromptSubmit dispatcher emit the same document.
func TestPathResolverRunMatchesDispatcher(t *testing.T) {
	for _, vendor := range []string{VendorClaude, VendorCopilot} {
		t.Run(vendor, func(t *testing.T) {
			vaulttest.Use(t) // hermetic CLAUDE_CONFIG_DIR + vault (harness hook switches)
			t.Setenv(hookspec.AgentEnv, vendor)
			t.Setenv("CLAUDE_RECALL_SEMANTIC", "0") // keep the co-dispatched memory-recall cheap
			dir := t.TempDir()
			f := filepath.Join(dir, "notes.md")
			if err := os.WriteFile(f, []byte("hello\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			payload := `{"prompt":"read ` + f + `"}`

			var directOut, directErr bytes.Buffer
			if code := Run("path-resolver", strings.NewReader(payload), &directOut, &directErr); code != ExitProceed {
				t.Fatalf("Run exit = %d (stderr %s)", code, &directErr)
			}
			var eventOut, eventErr bytes.Buffer
			if code := RunEvent("UserPromptSubmit", false, strings.NewReader(payload), &eventOut, &eventErr); code != ExitProceed {
				t.Fatalf("RunEvent exit = %d (stderr %s)", code, &eventErr)
			}
			if directOut.String() != eventOut.String() {
				t.Fatalf("direct %q != dispatched %q", &directOut, &eventOut)
			}
			doc := oneDoc(t, directOut.String())
			switch vendor {
			case VendorClaude:
				h, _ := doc["hookSpecificOutput"].(map[string]any)
				sys, _ := doc["systemMessage"].(string)
				ctx, _ := h["additionalContext"].(string)
				if sys == "" || ctx == "" {
					t.Errorf("claude doc = %v, want systemMessage + hookSpecificOutput.additionalContext", doc)
				}
			case VendorCopilot:
				ctx, _ := doc["additionalContext"].(string)
				if ctx == "" || len(doc) != 1 {
					t.Errorf("copilot doc = %v, want only additionalContext", doc)
				}
			}
		})
	}
}
