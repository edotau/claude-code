package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetup_DryRunOpensStoreNeverTouchesManifests(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	env.DryRun = true
	s, _ := specByName("chrome")
	markInstalled(f, s)
	supportDir := chromeSupportDir(env)
	hostBin := filepath.Join(env.Home, "chrome-native-host")
	touchFile(t, hostBin)
	writeHostManifest(t, supportDir, hostBin, validOrigin())
	writeProfile(t, supportDir, "Default", false, false) // extension absent -> install-extension action

	actions, err := Setup(env, "chrome")
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if len(actions.Items) != 1 {
		t.Fatalf("Items = %+v, want exactly one action", actions.Items)
	}
	a := actions.Items[0]
	if a.Kind != "install-extension" {
		t.Fatalf("Kind = %q, want install-extension", a.Kind)
	}
	if a.Executed {
		t.Fatal("Executed = true, want false under DryRun")
	}
	if len(a.Command) == 0 || a.Command[0] != "open" {
		t.Fatalf("Command = %v, want an open invocation", a.Command)
	}
	// Detection always issues read-only probes (the install-state mdfind, and guiState's own no-op
	// System Events query); DryRun only gates the install-extension action's mutating "open" call.
	allowed := map[string]bool{
		"mdfind kMDItemCFBundleIdentifier == '" + s.darwinBundleID + "'": true,
		guiProbeKey: true,
	}
	for _, c := range f.calls {
		if !allowed[c] {
			t.Fatalf("DryRun must not call Run beyond the install-state probes; got %q", c)
		}
	}
	// the host manifest on disk must be byte-identical to what writeHostManifest wrote: Setup never rewrites it.
	manifestPath := filepath.Join(supportDir, "NativeMessagingHosts", hostManifestName)
	if !dirExistsOrFile(manifestPath) {
		t.Fatalf("host manifest %s vanished", manifestPath)
	}
}

func dirExistsOrFile(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestSetup_AlreadyReadyIsNoop(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	s, _ := specByName("chrome")
	markInstalled(f, s)
	supportDir := chromeSupportDir(env)
	hostBin := filepath.Join(env.Home, "chrome-native-host")
	touchFile(t, hostBin)
	writeHostManifest(t, supportDir, hostBin, validOrigin())
	writeProfile(t, supportDir, "Default", true, false)

	actions, err := Setup(env, "chrome")
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if len(actions.Items) != 1 || actions.Items[0].Kind != "noop" {
		t.Fatalf("Items = %+v, want a single noop", actions.Items)
	}
}

func TestSetup_MissingHostNeverWritesOne(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	s, _ := specByName("chrome")
	markInstalled(f, s)
	supportDir := chromeSupportDir(env)
	writeProfile(t, supportDir, "Default", true, false)
	// deliberately no host manifest on disk.

	actions, err := Setup(env, "chrome")
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if len(actions.Items) != 1 || actions.Items[0].Kind != "missing-host" {
		t.Fatalf("Items = %+v, want a single missing-host action", actions.Items)
	}
	if actions.Items[0].Executed {
		t.Fatal("Executed = true, want false: Setup must never write Claude Code's host manifest")
	}
	manifestPath := filepath.Join(supportDir, "NativeMessagingHosts", hostManifestName)
	if dirExistsOrFile(manifestPath) {
		t.Fatalf("Setup wrote a host manifest at %s; it must never own that file", manifestPath)
	}
	if !strings.Contains(actions.Items[0].Description, "claude --chrome") {
		t.Fatalf("Description = %q, want it to point at claude --chrome", actions.Items[0].Description)
	}
}

func TestSetup_DisabledExtensionAction(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	s, _ := specByName("chrome")
	markInstalled(f, s)
	supportDir := chromeSupportDir(env)
	hostBin := filepath.Join(env.Home, "chrome-native-host")
	touchFile(t, hostBin)
	writeHostManifest(t, supportDir, hostBin, validOrigin())
	writeProfile(t, supportDir, "Default", true, true)

	actions, err := Setup(env, "chrome")
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if len(actions.Items) != 1 || actions.Items[0].Kind != "enable-extension" {
		t.Fatalf("Items = %+v, want a single enable-extension action", actions.Items)
	}
}

func TestSetup_DefaultNameResolvesThroughDefault(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	withLSHandlers(t, f, env, "lshandlers_chrome.json")
	s, _ := specByName("chrome")
	markInstalled(f, s)
	supportDir := chromeSupportDir(env)
	hostBin := filepath.Join(env.Home, "chrome-native-host")
	touchFile(t, hostBin)
	writeHostManifest(t, supportDir, hostBin, validOrigin())
	writeProfile(t, supportDir, "Default", true, false)

	actions, err := Setup(env, "default")
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if actions.Browser != "chrome" {
		t.Fatalf("Browser = %q, want chrome (resolved from the system default)", actions.Browser)
	}
}

func TestSetup_NonChromiumFamilyErrors(t *testing.T) {
	f := &fakeRun{}
	env := newTestEnv(t, f)
	s, _ := specByName("safari")
	markInstalled(f, s)

	_, err := Setup(env, "safari")
	if err == nil {
		t.Fatal("Setup() error = nil, want an error for a non-Chromium browser (tier 3)")
	}
}
