package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/ui/theme"
)

// hermeticTheme isolates the statusline config dir from the live ~/.claude.
func hermeticTheme(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
}

func configPath(dir string) string { return filepath.Join(dir, "state", "statusline", "config.json") }

func TestThemeSetPersistsAndReloadsThroughStatuslineLoader(t *testing.T) {
	hermeticTheme(t)
	names := theme.GradientNames()
	if len(names) == 0 {
		t.Fatal("no gradient palettes registered")
	}
	want := names[len(names)-1]
	code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdTheme([]string{want}) })
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
	code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdTheme([]string{"nord"}) })
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
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdTheme([]string{"ocean"}) }); code != 0 {
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

func TestThemePalettesListsEveryPaletteAndWritesNothing(t *testing.T) {
	dir := hermeticTheme(t)
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdTheme([]string{"--palettes"}) })
	if code != 0 {
		t.Fatalf("theme --palettes exit %d", code)
	}
	for _, name := range theme.GradientNames() {
		if !strings.Contains(out, name) {
			t.Errorf("preview missing palette %q:\n%s", name, out)
		}
	}
	if _, err := os.Stat(configPath(dir)); !os.IsNotExist(err) {
		t.Errorf("--palettes must not create config.json, stat err=%v", err)
	}
}

func TestThemeNoArgsPrintsHelpWithoutPalettes(t *testing.T) {
	dir := hermeticTheme(t)
	code, _, out := hosttest.CaptureOutput(t, func() int { return cmdTheme(nil) })
	if code != 0 {
		t.Fatalf("theme (no args) exit %d", code)
	}
	if !strings.Contains(out, "usage: claude-code theme") || strings.Contains(out, "PALETTES") {
		t.Errorf("bare theme must print usage only; palettes belong to --palettes:\n%s", out)
	}
	if _, err := os.Stat(configPath(dir)); !os.IsNotExist(err) {
		t.Error("bare `theme` must not write config.json")
	}
}

func TestThemeUnknownNameExitsTwoAndListsValid(t *testing.T) {
	dir := hermeticTheme(t)
	code, _, errOut := hosttest.CaptureOutput(t, func() int { return cmdTheme([]string{"not-a-real-palette"}) })
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

// TestThemeHelpExitsZeroWithoutRunning guards the flag.ErrHelp class of bug: discarding fs.Parse's
// error with `_ =` let `-h` print help AND then execute.
func TestThemeHelpExitsZeroWithoutRunning(t *testing.T) {
	dir := hermeticTheme(t)
	for _, flagArg := range []string{"-h", "--help"} {
		code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdTheme([]string{flagArg}) })
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
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdTheme([]string{"statusline", "--style", "minimal"}) })
	if code != 0 {
		t.Fatalf("theme statusline exit %d", code)
	}
	if !strings.Contains(out, "Probe-Model-9") {
		t.Errorf("theme statusline output lacks the stdin model name: %q", out)
	}
}

func TestThemeStatuslineBadFlagExitsTwo(t *testing.T) {
	hermeticTheme(t)
	code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdTheme([]string{"statusline", "--nope"}) })
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}

func TestThemeHelpListsNewSubVerbs(t *testing.T) {
	hermeticTheme(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdTheme([]string{"-h"}) })
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdTheme([]string{"vscode", "--bogus-flag"}) })
	if code != 2 {
		t.Errorf("exit %d, want 2 (flag parse error from vscode.Run)", code)
	}
	if strings.Contains(stderr, "claude-code vscode") {
		t.Errorf("stale old verb name in help/errors: %q", stderr)
	}
}

func TestThemeResetDeletesOverrideAndIsIdempotent(t *testing.T) {
	dir := hermeticTheme(t)
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdTheme([]string{"nord"}) }); code != 0 {
		t.Fatalf("theme nord exit %d", code)
	}
	for i, want := range []string{"override removed", "already on the default"} {
		code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdTheme([]string{"--reset"}) })
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("reset #%d: exit %d, stdout %q, want %q", i+1, code, out, want)
		}
		if _, err := os.Stat(configPath(dir)); !os.IsNotExist(err) {
			t.Fatalf("reset #%d: config.json still present (stat err %v)", i+1, err)
		}
	}
}
