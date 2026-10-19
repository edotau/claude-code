package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/statusline"
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
	os.Stdout, os.Stderr = wo, we
	code = fn()
	wo.Close()
	we.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	var bo, be bytes.Buffer
	io.Copy(&bo, ro)
	io.Copy(&be, re)
	return code, bo.String(), be.String()
}

func configPath(dir string) string { return filepath.Join(dir, "statusline", "config.json") }

func TestThemeSetPersistsAndReloadsThroughStatuslineLoader(t *testing.T) {
	hermeticTheme(t)
	names := statusline.GradientNames()
	if len(names) == 0 {
		t.Fatal("no gradient palettes registered")
	}
	want := names[len(names)-1]
	code, _, _ := captureOutput(t, func() int { return cmdTheme([]string{want}) })
	if code != 0 {
		t.Fatalf("theme %s exit %d", want, code)
	}
	// Re-read via statusline's own public loader, not our just-written file.
	_, stops, _ := statusline.Theme()
	if got := statusline.GradientStops(want); !reflect.DeepEqual(got, stops) {
		t.Errorf("statusline.Theme() stops = %v, want %s's stops %v", stops, want, got)
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
	for _, name := range statusline.GradientNames() {
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
	code, out, _ := captureOutput(t, func() int { return cmdTheme(nil) })
	if code != 0 {
		t.Fatalf("theme (no args) exit %d", code)
	}
	for _, name := range statusline.GradientNames() {
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
	for _, name := range statusline.GradientNames() {
		if !strings.Contains(errOut, name) {
			t.Errorf("unknown-name message missing valid palette %q:\n%s", name, errOut)
		}
	}
	if _, err := os.Stat(configPath(dir)); !os.IsNotExist(err) {
		t.Error("an unknown palette name must not write config.json")
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
