package browser

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// lsHandlersPath mirrors defaultDarwin's own path construction so the fake run's key matches.
func lsHandlersPath(home string) string {
	return filepath.Join(home, "Library", "Preferences", "com.apple.LaunchServices", "com.apple.launchservices.secure.plist")
}

func withLSHandlers(t *testing.T, f *fakeRun, env Env, fixture string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	key := "plutil -convert json -o - " + lsHandlersPath(env.Home)
	if f.answers == nil {
		f.answers = map[string]fakeAnswer{}
	}
	f.answers[key] = fakeAnswer{out: string(data)}
}

func TestDefault_DarwinChromium(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	withLSHandlers(t, f, env, "lshandlers_chrome.json")

	b, err := Default(env)
	if err != nil {
		t.Fatalf("Default() error = %v, want nil for a Chromium default", err)
	}
	if b.Name != "chrome" {
		t.Fatalf("Default() Name = %q, want chrome", b.Name)
	}
	if !b.Default {
		t.Fatal("Default() Browser.Default = false, want true")
	}
}

func TestDefault_DarwinNonChromiumUnsupported(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	withLSHandlers(t, f, env, "lshandlers_safari.json")

	b, err := Default(env)
	if err == nil {
		t.Fatal("Default() error = nil, want ErrDefaultUnsupported for a Safari default")
	}
	if !errors.Is(err, ErrDefaultUnsupported) {
		t.Fatalf("Default() error = %v, want it to wrap ErrDefaultUnsupported", err)
	}
	if b.Name != "safari" {
		t.Fatalf("Default() still returned Browser.Name = %q, want safari (identity preserved on tier-3 unsupported)", b.Name)
	}
}

func TestDefault_DarwinUnrecognizedBundleID(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	key := "plutil -convert json -o - " + lsHandlersPath(env.Home)
	f.answers = map[string]fakeAnswer{
		key: {out: `{"LSHandlers":[{"LSHandlerURLScheme":"https","LSHandlerRoleAll":"com.example.unknownbrowser"}]}`},
	}

	b, err := Default(env)
	if err == nil {
		t.Fatal("Default() error = nil, want an error for an unrecognized bundle id")
	}
	if b.Name != "" {
		t.Fatalf("Default() Browser = %+v, want zero value for an unrecognized bundle id", b)
	}
}

func TestDefault_LinuxXdgSettings(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	env.GOOS = "linux"
	f.answers = map[string]fakeAnswer{
		"xdg-settings get default-web-browser": {out: "google-chrome.desktop\n"},
	}

	b, err := Default(env)
	if err != nil {
		t.Fatalf("Default() error = %v, want nil", err)
	}
	if b.Name != "chrome" {
		t.Fatalf("Default() Name = %q, want chrome", b.Name)
	}
}

func TestDetect_DefaultFlagPropagatesToReport(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	withLSHandlers(t, f, env, "lshandlers_chrome.json")
	// chrome also needs to read as installed/ready for a complete, realistic report.
	s, _ := specByName("chrome")
	markInstalled(f, s)

	report := Detect(env)
	if report.Default != "chrome" {
		t.Fatalf("Report.Default = %q, want chrome", report.Default)
	}
	chrome := findBrowser(t, report, "chrome")
	if !chrome.Default {
		t.Fatal("chrome.Default = false, want true")
	}
	for _, b := range report.Browsers {
		if b.Name != "chrome" && b.Default {
			t.Fatalf("%s.Default = true, want only chrome flagged", b.Name)
		}
	}
}
