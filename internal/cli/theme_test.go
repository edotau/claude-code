package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/ui/theme"
)

// hermeticTheme isolates the statusline config dir from the live ~/.claude.
func hermeticTheme(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
}

// captureOutput redirects os.Stdout/os.Stderr around fn and returns fn's exit code plus both streams.
func captureOutput(t *testing.T, fn func() int) (code int, stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var bo, be bytes.Buffer
	var drained sync.WaitGroup
	drained.Go(func() { io.Copy(&bo, ro) }) // drain while fn runs: a full pipe would block its writes forever
	drained.Go(func() { io.Copy(&be, re) })
	os.Stdout, os.Stderr = wo, we
	code = fn()
	wo.Close()
	we.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	drained.Wait()
	return code, bo.String(), be.String()
}

// A command printing past the pipe buffer must not deadlock: the audit hung 5m on a large settings render.
func TestCaptureOutputDrainsPastThePipeBuffer(t *testing.T) {
	want := 256 << 10
	code, stdout, _ := captureOutput(t, func() int {
		if _, err := os.Stdout.Write(make([]byte, want)); err != nil {
			return 1
		}
		return 0
	})
	if code != 0 || len(stdout) != want {
		t.Fatalf("code %d, captured %d bytes, want %d", code, len(stdout), want)
	}
}

func configPath(dir string) string { return filepath.Join(dir, "state", "statusline", "config.json") }

func TestThemeSetPersistsAndReloadsThroughStatuslineLoader(t *testing.T) {
	hermeticTheme(t)
	names := theme.GradientNames()
	if len(names) == 0 {
		t.Fatal("no gradient palettes registered")
	}
	want := names[len(names)-1]
	code, _, _ := captureOutput(t, func() int { return cmdTheme([]string{want}) })
	if code != 0 {
		t.Fatalf("theme %s exit %d", want, code)
	}
	// Re-read via statusline's own public loader, not our just-written file.
	_, stops, _ := theme.Theme()
	if got := theme.GradientStops(want); !reflect.DeepEqual(got, stops) {
		t.Errorf("theme.Theme() stops = %v, want %s's stops %v", stops, want, got)
	}
}

func TestThemeSetMergesWithoutDisturbingOtherKeys(t *testing.T) {
	dir := hermeticTheme(t)
	code, _, _ := captureOutput(t, func() int { return cmdTheme([]string{"nord"}) })
	if code != 0 {
		t.Fatalf("theme nord exit %d", code)
	}
	raw, err := os.ReadFile(configPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["gradient"] != "nord" {
		t.Errorf("gradient = %v, want %q", m["gradient"], "nord")
	}
	for _, key := range []string{"style", "bar", "segments", "dashboard"} {
		if _, ok := m[key]; !ok {
			t.Errorf("merge dropped pre-existing key %q", key)
		}
	}
}

func TestThemeSetOnSecondCallPreservesFirstCallsOtherEdits(t *testing.T) {
	dir := hermeticTheme(t)
	// Simulate a user override file with a customized, non-default key alongside the palette.
	seed := `{"style":"gradient","gradient":"gemini","charset":"nerd"}`
	if err := os.MkdirAll(filepath.Dir(configPath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath(dir), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := captureOutput(t, func() int { return cmdTheme([]string{"ocean"}) }); code != 0 {
		t.Fatalf("theme ocean exit %d", code)
	}
	raw, err := os.ReadFile(configPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["gradient"] != "ocean" {
		t.Errorf("gradient = %v, want ocean", m["gradient"])
	}
	if m["charset"] != "nerd" {
		t.Errorf("charset = %v, want the pre-existing nerd to survive the merge", m["charset"])
	}
}

func TestThemePreviewListsEveryPaletteAndWritesNothing(t *testing.T) {
	dir := hermeticTheme(t)
	code, out, _ := captureOutput(t, func() int { return cmdTheme([]string{"--preview"}) })
	if code != 0 {
		t.Fatalf("theme --preview exit %d", code)
	}
	for _, name := range theme.GradientNames() {
		if !strings.Contains(out, name) {
			t.Errorf("preview missing palette %q:\n%s", name, out)
		}
	}
	if _, err := os.Stat(configPath(dir)); !os.IsNotExist(err) {
		t.Errorf("--preview must not create config.json, stat err=%v", err)
	}
}

func TestThemeNoArgsAlsoListsAndWritesNothing(t *testing.T) {
	dir := hermeticTheme(t)
	code, _, out := captureOutput(t, func() int { return cmdTheme(nil) })
	if code != 0 {
		t.Fatalf("theme (no args) exit %d", code)
	}
	// bare `theme` is its help: usage, then the palettes with the current one marked
	if !strings.Contains(out, "usage: claude-code theme") || !strings.Contains(out, "● "+currentGradient()) {
		t.Errorf("bare theme must print usage and mark the current palette:\n%s", out)
	}
	for _, name := range theme.GradientNames() {
		if !strings.Contains(out, name) {
			t.Errorf("no-arg listing missing palette %q:\n%s", name, out)
		}
	}
	if _, err := os.Stat(configPath(dir)); !os.IsNotExist(err) {
		t.Error("bare `theme` must not write config.json")
	}
}

func TestThemeUnknownNameExitsTwoAndListsValid(t *testing.T) {
	dir := hermeticTheme(t)
	code, _, errOut := captureOutput(t, func() int { return cmdTheme([]string{"not-a-real-palette"}) })
	if code != 2 {
		t.Fatalf("theme not-a-real-palette exit %d, want 2", code)
	}
	for _, name := range theme.GradientNames() {
		if !strings.Contains(errOut, name) {
			t.Errorf("unknown-name message missing valid palette %q:\n%s", name, errOut)
		}
	}
	if _, err := os.Stat(configPath(dir)); !os.IsNotExist(err) {
		t.Error("an unknown palette name must not write config.json")
	}
}

// TestSetGradientRejectsNullConfigWithoutPanicking: json.Unmarshal("null", &m) succeeds but leaves m
// nil — a bare `if err != nil` check would miss it and panic on the m["gradient"] = assignment.
// setGradient must error out instead, and must not touch the file.
func TestSetGradientRejectsNullConfigWithoutPanicking(t *testing.T) {
	dir := hermeticTheme(t)
	path := configPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("null"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := setGradient("ocean")
	if err == nil {
		t.Fatal("setGradient on a null config: err = nil, want an error")
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != "null" {
		t.Errorf("setGradient on a null config changed the file: %q", raw)
	}
}

// TestThemeHelpExitsZeroWithoutRunning guards the flag.ErrHelp class of bug: discarding fs.Parse's
// error with `_ =` let `-h` print help AND then execute.
func TestThemeHelpExitsZeroWithoutRunning(t *testing.T) {
	dir := hermeticTheme(t)
	for _, flagArg := range []string{"-h", "--help"} {
		code, _, _ := captureOutput(t, func() int { return cmdTheme([]string{flagArg}) })
		if code != 0 {
			t.Errorf("theme %s exit %d, want 0 — asking for help is not an error", flagArg, code)
		}
	}
	if _, err := os.Stat(configPath(dir)); !os.IsNotExist(err) {
		t.Error("-h/--help must not write config.json")
	}
}

func TestThemeStatuslineRendersFromStdin(t *testing.T) {
	hermeticTheme(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(`{"model":{"display_name":"Probe-Model-9"},"workspace":{"current_dir":"/tmp/probe"}}`); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	code, out, _ := captureOutput(t, func() int { return cmdTheme([]string{"statusline", "--style", "minimal"}) })
	if code != 0 {
		t.Fatalf("theme statusline exit %d", code)
	}
	if !strings.Contains(out, "Probe-Model-9") {
		t.Errorf("theme statusline output lacks the stdin model name: %q", out)
	}
}

func TestThemeStatuslineBadFlagExitsTwo(t *testing.T) {
	hermeticTheme(t)
	code, _, _ := captureOutput(t, func() int { return cmdTheme([]string{"statusline", "--nope"}) })
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}

func TestThemeHelpListsNewSubVerbs(t *testing.T) {
	hermeticTheme(t)
	code, _, stderr := captureOutput(t, func() int { return cmdTheme([]string{"-h"}) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"apply", "statusline", "vscode"} {
		if !strings.Contains(stderr, "\n  "+want+" ") {
			t.Errorf("theme -h lacks %q: %q", want, stderr)
		}
	}
}

func TestThemeVSCodeDispatchesToVSCode(t *testing.T) {
	hermeticTheme(t)
	code, _, stderr := captureOutput(t, func() int { return cmdTheme([]string{"vscode", "--bogus-flag"}) })
	if code != 2 {
		t.Errorf("exit %d, want 2 (flag parse error from cmdVSCode)", code)
	}
	if strings.Contains(stderr, "claude-code vscode") {
		t.Errorf("stale old verb name in help/errors: %q", stderr)
	}
}
