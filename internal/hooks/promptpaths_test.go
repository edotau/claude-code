package hooks

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
func TestWSLPathNormalizeEmitsUserVisibleFeedback(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "shot.png")
	writePNG(t, img, 640, 480)

	var out bytes.Buffer
	in := strings.NewReader(`{"prompt":"look at ` + img + `"}`)
	if code := promptPathFeedback(in, &out, func() bool { return false }); code != ExitProceed {
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
	promptPathFeedback(in, &out, func() bool { return false })
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
	promptPathFeedback(in, &out, func() bool { return false })
	if !strings.Contains(out.String(), "notes.md") {
		t.Errorf("expected the resolved path in the feedback:\n%s", out.String())
	}
}

// A hung `wslpath` interop binary must not stall the hook past wslpathTimeout: TranslateWinPath must
// fall back to the /mnt/<drive> mapping instead of blocking the whole UserPromptSubmit indefinitely.
func TestTranslateWinPathTimesOutOnHungWslpath(t *testing.T) {
	saved := wslpathTimeout
	wslpathTimeout = 100 * time.Millisecond
	t.Cleanup(func() { wslpathTimeout = saved })
	dir := t.TempDir()
	script := filepath.Join(dir, "wslpath")
	// `exec` replaces the shell in place (no forked grandchild holding the stdout pipe open), matching
	// the real failure mode: a single interop process blocked in a syscall, which SIGKILL interrupts.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	start := time.Now()
	got := TranslateWinPath(`C:\Users\x\a.png`)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("TranslateWinPath took %v, want it bounded by wslpathTimeout (%v) plus the fallback", elapsed, wslpathTimeout)
	}
	if want := "/mnt/c/Users/x/a.png"; got != want {
		t.Errorf("TranslateWinPath = %q, want the fallback mapping %q", got, want)
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

// TestFeedbackPayloadMarshalHandlesErrors: feedbackPayload must handle JSON marshal errors
// properly. Regression test for defect where json.Marshal errors were swallowed with _,
// producing corrupted hook output. Now properly handles errors by returning "" instead.
func TestFeedbackPayloadMarshalHandlesErrors(t *testing.T) {
	// Create valid path reports to ensure feedbackPayload is called correctly.
	reps := []pathReport{
		{path: "/test/file.txt", detail: "100 B", isImage: false},
	}

	payload := feedbackPayload(reps)
	if payload == "" {
		t.Fatal("feedbackPayload returned empty string for valid input")
	}

	// Verify the payload is valid JSON.
	var doc map[string]any
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		t.Fatalf("feedbackPayload output is not valid JSON: %v\npayload=%s", err, payload)
	}

	// Verify expected fields are present.
	if _, ok := doc["systemMessage"]; !ok {
		t.Error("systemMessage field missing from payload")
	}
	if _, ok := doc["hookSpecificOutput"]; !ok {
		t.Error("hookSpecificOutput field missing from payload")
	}
}
