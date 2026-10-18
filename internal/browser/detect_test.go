package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRun dispatches canned stdout/err per "name arg1 arg2…" key, and records every call made.
type fakeRun struct {
	calls   []string
	answers map[string]fakeAnswer
}

type fakeAnswer struct {
	out string
	err error
}

func (f *fakeRun) run(name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if a, ok := f.answers[key]; ok {
		return a.out, a.err
	}
	// mdfind/command -v default to "not found" so an unconfigured browser reads not-installed.
	return "", nil
}

// newTestEnv builds a darwin Env rooted at a fresh t.TempDir(); nothing under ~/Library is touched.
func newTestEnv(t *testing.T, f *fakeRun) Env {
	t.Helper()
	home := t.TempDir()
	appSupport := filepath.Join(home, "Library", "Application Support")
	if err := os.MkdirAll(appSupport, 0o755); err != nil {
		t.Fatal(err)
	}
	return Env{
		Home:       home,
		AppSupport: appSupport,
		Run:        f.run,
		Now:        func() time.Time { return time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC) },
		GOOS:       "darwin",
	}
}

// markInstalled tells isInstalled's mdfind call for s.name to report the browser present.
func markInstalled(f *fakeRun, s spec) {
	key := "mdfind kMDItemCFBundleIdentifier == '" + s.darwinBundleID + "'"
	if f.answers == nil {
		f.answers = map[string]fakeAnswer{}
	}
	f.answers[key] = fakeAnswer{out: "/Applications/" + s.displayName + ".app"}
}

// writeHostManifest writes a NativeMessagingHosts manifest for supportDir; hostBinary must exist on
// disk (checkHost os.Stat's it) and origin lets a test inject a mismatched allowed_origins.
func writeHostManifest(t *testing.T, supportDir, hostBinary, origin string) {
	t.Helper()
	dir := filepath.Join(supportDir, "NativeMessagingHosts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := hostManifest{Path: hostBinary, AllowedOrigins: []string{origin}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hostManifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// touchFile creates an empty file at path (and parents), standing in for the native-host binary.
func touchFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeProfile creates supportDir/<profile>/Extensions/<id> (when hasExtension) and a Preferences
// file marking it disabled when disabled is true.
func writeProfile(t *testing.T, supportDir, profile string, hasExtension, disabled bool) {
	t.Helper()
	profileDir := filepath.Join(supportDir, profile)
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if hasExtension {
		if err := os.MkdirAll(filepath.Join(profileDir, "Extensions", extensionID), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	state := 1
	if disabled {
		state = 0
	}
	prefs := map[string]any{
		"extensions": map[string]any{
			"settings": map[string]any{
				extensionID: map[string]any{"state": state},
			},
		},
	}
	data, err := json.Marshal(prefs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "Preferences"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func validOrigin() string {
	return "chrome-extension://" + extensionID + "/"
}

func chromeSupportDir(env Env) string {
	s, _ := specByName("chrome")
	return appSupportPath(env, s)
}

func TestDetect_ChromeReadyExtensionEnabled(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	s, _ := specByName("chrome")
	markInstalled(f, s)
	supportDir := chromeSupportDir(env)
	hostBin := filepath.Join(env.Home, "chrome-native-host")
	touchFile(t, hostBin)
	writeHostManifest(t, supportDir, hostBin, validOrigin())
	writeProfile(t, supportDir, "Default", true, false)

	report := Detect(env)
	b := findBrowser(t, report, "chrome")
	if !b.Installed {
		t.Fatal("want Installed=true")
	}
	if b.Host != stateReady {
		t.Fatalf("Host = %q, want ready", b.Host)
	}
	if b.Paths.Extension != stateReady {
		t.Fatalf("Paths.Extension = %q, want ready", b.Paths.Extension)
	}
	if len(b.Profiles) != 1 || b.Profiles[0].Extension != stateReady {
		t.Fatalf("Profiles = %+v, want one ready profile", b.Profiles)
	}
}

func TestDetect_HostManifestMissing(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	s, _ := specByName("chrome")
	markInstalled(f, s)
	supportDir := chromeSupportDir(env)
	writeProfile(t, supportDir, "Default", true, false)
	// no host manifest written at all.

	report := Detect(env)
	b := findBrowser(t, report, "chrome")
	if b.Host != stateNoHost {
		t.Fatalf("Host = %q, want no-host", b.Host)
	}
	if b.Paths.Extension != stateNoHost {
		t.Fatalf("Paths.Extension = %q, want no-host even though the extension dir exists", b.Paths.Extension)
	}
}

func TestDetect_HostManifestBadPath(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	s, _ := specByName("chrome")
	markInstalled(f, s)
	supportDir := chromeSupportDir(env)
	// manifest points at a binary that does not exist on disk.
	writeHostManifest(t, supportDir, filepath.Join(env.Home, "does-not-exist"), validOrigin())
	writeProfile(t, supportDir, "Default", true, false)

	report := Detect(env)
	b := findBrowser(t, report, "chrome")
	if b.Host != stateNoHost {
		t.Fatalf("Host = %q, want no-host when the manifest's path target is missing", b.Host)
	}
}

func TestDetect_HostManifestWrongOrigin(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	s, _ := specByName("chrome")
	markInstalled(f, s)
	supportDir := chromeSupportDir(env)
	hostBin := filepath.Join(env.Home, "chrome-native-host")
	touchFile(t, hostBin)
	writeHostManifest(t, supportDir, hostBin, "chrome-extension://wrongidwrongidwrongidwrongidwr/")
	writeProfile(t, supportDir, "Default", true, false)

	report := Detect(env)
	b := findBrowser(t, report, "chrome")
	if b.Host != stateNoHost {
		t.Fatalf("Host = %q, want no-host on an allowed_origins mismatch", b.Host)
	}
}

func TestDetect_ExtensionDisabled(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	s, _ := specByName("chrome")
	markInstalled(f, s)
	supportDir := chromeSupportDir(env)
	hostBin := filepath.Join(env.Home, "chrome-native-host")
	touchFile(t, hostBin)
	writeHostManifest(t, supportDir, hostBin, validOrigin())
	writeProfile(t, supportDir, "Default", true, true)

	report := Detect(env)
	b := findBrowser(t, report, "chrome")
	if b.Profiles[0].Extension != stateDisabled {
		t.Fatalf("profile Extension = %q, want disabled", b.Profiles[0].Extension)
	}
	if b.Paths.Extension != stateDisabled {
		t.Fatalf("Paths.Extension = %q, want disabled (no ready profile, one disabled)", b.Paths.Extension)
	}
}

func TestDetect_ExtensionAbsent(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	s, _ := specByName("chrome")
	markInstalled(f, s)
	supportDir := chromeSupportDir(env)
	hostBin := filepath.Join(env.Home, "chrome-native-host")
	touchFile(t, hostBin)
	writeHostManifest(t, supportDir, hostBin, validOrigin())
	writeProfile(t, supportDir, "Default", false, false)

	report := Detect(env)
	b := findBrowser(t, report, "chrome")
	if b.Profiles[0].Extension != stateNoExtension {
		t.Fatalf("profile Extension = %q, want no-extension", b.Profiles[0].Extension)
	}
	if b.Paths.Extension != stateNoExtension {
		t.Fatalf("Paths.Extension = %q, want no-extension", b.Paths.Extension)
	}
}

func TestDetect_MultipleProfilesOneReadyWins(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	s, _ := specByName("chrome")
	markInstalled(f, s)
	supportDir := chromeSupportDir(env)
	hostBin := filepath.Join(env.Home, "chrome-native-host")
	touchFile(t, hostBin)
	writeHostManifest(t, supportDir, hostBin, validOrigin())
	writeProfile(t, supportDir, "Default", true, false)
	writeProfile(t, supportDir, "Profile 1", false, false)

	report := Detect(env)
	b := findBrowser(t, report, "chrome")
	if len(b.Profiles) != 2 {
		t.Fatalf("Profiles = %+v, want 2", b.Profiles)
	}
	if b.Profiles[0].Name != "Default" || b.Profiles[1].Name != "Profile 1" {
		t.Fatalf("Profiles order = %+v, want Default then Profile 1", b.Profiles)
	}
	if b.Paths.Extension != stateReady {
		t.Fatalf("Paths.Extension = %q, want ready (one profile is ready)", b.Paths.Extension)
	}
}

func TestDetect_NotInstalled(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	// mdfind never configured to return anything for chrome -> not installed.

	report := Detect(env)
	b := findBrowser(t, report, "chrome")
	if b.Installed {
		t.Fatal("want Installed=false")
	}
	if b.Host != stateNotInstalled {
		t.Fatalf("Host = %q, want not-installed", b.Host)
	}
	if b.Paths != (Paths{Extension: stateNotInstalled, CDP: stateNotInstalled, WebDriver: stateNotInstalled, GUI: stateNotInstalled}) {
		t.Fatalf("Paths = %+v, want all not-installed", b.Paths)
	}
}

// guiProbeKey is the osascript probe guiState issues, keyed the same way fakeRun.run joins its args.
const guiProbeKey = `osascript -e tell application "System Events" to get name of first process`

type fakeExecErr string

func (e fakeExecErr) Error() string { return string(e) }

func TestGUIState_AssistiveAccessErrorIsDisabled(t *testing.T) {
	f := &fakeRun{answers: map[string]fakeAnswer{
		guiProbeKey: {err: fakeExecErr("execution error: Not authorized to send Apple events (-1719)")},
	}}
	env := newTestEnv(t, f)
	if got := guiState(env); got != stateDisabled {
		t.Errorf("guiState = %q, want %q on an assistive-access error", got, stateDisabled)
	}
}

func TestGUIState_OtherErrorStaysAvailable(t *testing.T) {
	f := &fakeRun{answers: map[string]fakeAnswer{
		guiProbeKey: {err: fakeExecErr("osascript: command not found")},
	}}
	env := newTestEnv(t, f)
	if got := guiState(env); got != stateAvailable {
		t.Errorf("guiState = %q, want %q when the failure is not the specific assistive-access error", got, stateAvailable)
	}
}

func TestGUIState_NoErrorIsAvailable(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	if got := guiState(env); got != stateAvailable {
		t.Errorf("guiState = %q, want %q on a clean probe", got, stateAvailable)
	}
}

func TestGUIState_NonDarwinIsUnsupported(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	env.GOOS = "linux"
	if got := guiState(env); got != stateUnsupported {
		t.Errorf("guiState = %q, want %q off darwin", got, stateUnsupported)
	}
}

func TestDetect_UnsupportedGOOS(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	env.GOOS = "windows"

	report := Detect(env)
	b := findBrowser(t, report, "chrome")
	if b.Host != stateUnsupported {
		t.Fatalf("Host = %q, want unsupported on an unhandled GOOS", b.Host)
	}
	if b.Paths != (Paths{Extension: stateUnsupported, CDP: stateUnsupported, WebDriver: stateUnsupported, GUI: stateUnsupported}) {
		t.Fatalf("Paths = %+v, want all unsupported", b.Paths)
	}
}

func TestDetect_NoRealCommandsRun(t *testing.T) {
	// Guards the harness rule: Detect must only ever go through env.Run, never exec a real binary.
	f := &fakeRun{}
	env := newTestEnv(t, f)
	markInstalled(f, specMust(t, "chrome"))
	markInstalled(f, specMust(t, "safari"))
	Detect(env)
	for _, c := range f.calls {
		if strings.HasPrefix(c, "open ") {
			t.Fatalf("Detect must never call open; got %q", c)
		}
	}
}

func specMust(t *testing.T, name string) spec {
	t.Helper()
	s, ok := specByName(name)
	if !ok {
		t.Fatalf("no spec named %q", name)
	}
	return s
}

func findBrowser(t *testing.T, report Report, name string) Browser {
	t.Helper()
	for _, b := range report.Browsers {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("no browser named %q in report: %+v", name, report)
	return Browser{}
}
