package browser

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Detect probes every known browser plus the system default and returns one report.
func Detect(env Env) Report {
	browsers := make([]Browser, 0, len(knownBrowsers))
	for _, s := range knownBrowsers {
		browsers = append(browsers, detectOne(env, s))
	}
	def, _ := Default(env) // ErrDefaultUnsupported still carries the resolved (non-Chromium) name
	if def.Name != "" {
		for i := range browsers {
			browsers[i].Default = browsers[i].Name == def.Name
		}
	}
	return Report{Browsers: browsers, Default: def.Name, GeneratedAt: env.now()}
}

// detectOne builds one browser's full result: identity, install state, then its control paths.
func detectOne(env Env, s spec) Browser {
	b := Browser{Name: s.name, DisplayName: s.displayName, Family: s.family}
	if env.WSL && winExePath(env, s.name) != "" { // WSL: the Windows host's browser, reached over CDP
		detectWindows(env, s, &b)
		return b
	}
	switch env.goos() {
	case "darwin":
		b.BundleID = s.darwinBundleID
	case "linux":
		// no stable identifier surfaced for Linux; Name/DisplayName are enough to act on.
	default:
		return stubUnsupported(b) // Windows and anything else: stubbed, not probed
	}
	b.Installed = isInstalled(env, s)
	if !b.Installed {
		return stubNotInstalled(b)
	}
	switch s.family {
	case familyChromium:
		detectChromium(env, s, &b)
	case familySafari:
		detectSafari(env, &b)
	case familyFirefox:
		detectFirefox(env, &b)
	}
	return b
}

func stubUnsupported(b Browser) Browser {
	b.Host = stateUnsupported
	b.Paths = Paths{Extension: stateUnsupported, CDP: stateUnsupported, WebDriver: stateUnsupported, GUI: stateUnsupported}
	return b
}

func stubNotInstalled(b Browser) Browser {
	b.Host = stateNotInstalled
	b.Paths = Paths{Extension: stateNotInstalled, CDP: stateNotInstalled, WebDriver: stateNotInstalled, GUI: stateNotInstalled}
	return b
}

// isInstalled checks the app bundle (macOS, via mdfind) or the binary on PATH (Linux).
func isInstalled(env Env, s spec) bool {
	switch env.goos() {
	case "darwin":
		if s.darwinBundleID == "" {
			return false
		}
		out, err := env.run("mdfind", "kMDItemCFBundleIdentifier == '"+s.darwinBundleID+"'")
		return err == nil && strings.TrimSpace(out) != ""
	case "linux":
		if s.linuxBinary == "" {
			return false
		}
		out, err := env.run("sh", "-c", "command -v "+s.linuxBinary)
		return err == nil && strings.TrimSpace(out) != ""
	default:
		return false
	}
}

// detectChromium fills in a Chromium-family browser's host, profiles, and the four control paths.
func detectChromium(env Env, s spec, b *Browser) {
	supportDir := appSupportPath(env, s)
	b.Profiles = detectProfiles(supportDir)
	if checkHost(supportDir) {
		b.Host = stateReady
		b.Paths.Extension = rollupExtension(b.Profiles)
	} else {
		b.Host = stateNoHost
		b.Paths.Extension = stateNoHost
	}
	b.Paths.CDP = stateAvailable // binary present, confirmed by isInstalled above
	b.Paths.WebDriver = stateUnsupported
	b.Paths.GUI = guiState(env)
}

// checkHost reports whether supportDir/NativeMessagingHosts/<manifest> is present, its path target
// exists, and its allowed_origins matches this extension — the three things Claude Code must have
// already wired for the extension control path to be reachable at all.
func checkHost(supportDir string) bool {
	path := filepath.Join(supportDir, "NativeMessagingHosts", hostManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var m hostManifest
	if json.Unmarshal(data, &m) != nil || m.Path == "" {
		return false
	}
	if _, err := os.Stat(m.Path); err != nil {
		return false
	}
	want := "chrome-extension://" + extensionID + "/"
	for _, origin := range m.AllowedOrigins {
		if origin == want {
			return true
		}
	}
	return false
}

// detectProfiles lists supportDir/Default and supportDir/Profile * and the extension state in each.
func detectProfiles(supportDir string) []Profile {
	var names []string
	if dirExists(filepath.Join(supportDir, "Default")) {
		names = append(names, "Default")
	}
	if matches, err := filepath.Glob(filepath.Join(supportDir, "Profile *")); err == nil {
		for _, m := range matches {
			if dirExists(m) {
				names = append(names, filepath.Base(m))
			}
		}
	}
	sort.Strings(names)
	profiles := make([]Profile, 0, len(names))
	for _, name := range names {
		profiles = append(profiles, Profile{Name: name, Extension: profileExtensionState(filepath.Join(supportDir, name))})
	}
	return profiles
}

// profileExtensionState: the extension dir must exist under this profile; Preferences can still mark it disabled.
func profileExtensionState(profileDir string) string {
	if !dirExists(filepath.Join(profileDir, "Extensions", extensionID)) {
		return stateNoExtension
	}
	if extensionDisabled(filepath.Join(profileDir, "Preferences")) {
		return stateDisabled
	}
	return stateReady
}

// extensionDisabled reads Preferences -> extensions.settings.<id>.state; absent/non-zero counts as enabled.
func extensionDisabled(prefsPath string) bool {
	data, err := os.ReadFile(prefsPath)
	if err != nil {
		return false
	}
	var doc struct {
		Extensions struct {
			Settings map[string]struct {
				State *int `json:"state"`
			} `json:"settings"`
		} `json:"extensions"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return false
	}
	s, ok := doc.Extensions.Settings[extensionID]
	return ok && s.State != nil && *s.State == 0
}

// rollupExtension is the browser-level Paths.Extension: ready if any profile is, else disabled if any
// profile has the extension but off, else no-extension.
func rollupExtension(profiles []Profile) string {
	disabled := false
	for _, p := range profiles {
		if p.Extension == stateReady {
			return stateReady
		}
		if p.Extension == stateDisabled {
			disabled = true
		}
	}
	if disabled {
		return stateDisabled
	}
	return stateNoExtension
}

func detectSafari(env Env, b *Browser) {
	b.Host = stateUnsupported
	b.Paths = Paths{
		Extension: stateUnsupported,
		CDP:       stateUnsupported,
		WebDriver: safariWebDriverState(env),
		GUI:       guiState(env),
	}
}

// safariWebDriverState reads com.apple.Safari's AllowRemoteAutomation preference (read-only); the
// mutating `safaridriver --enable` is never invoked here — see Setup's doc comment on what this
// package may act on versus only report.
func safariWebDriverState(env Env) string {
	out, err := env.run("defaults", "read", "com.apple.Safari", "AllowRemoteAutomation")
	if err == nil && strings.TrimSpace(out) == "1" {
		return stateAvailable
	}
	return stateDisabled
}

func detectFirefox(env Env, b *Browser) {
	b.Host = stateUnsupported
	b.Paths = Paths{
		Extension: stateUnsupported,
		CDP:       stateUnsupported,
		WebDriver: geckodriverState(env),
		GUI:       guiState(env),
	}
}

func geckodriverState(env Env) string {
	out, err := env.run("sh", "-c", "command -v geckodriver")
	if err == nil && strings.TrimSpace(out) != "" {
		return stateAvailable
	}
	return stateDisabled
}

// guiState probes gui control read-only: on macOS it needs Accessibility/Apple-Events permission for the
// controlling app, checked with a no-op System Events query that never performs a UI action (so it cannot
// itself trigger the permission prompt); a -1719/-25211 error means that permission was already denied.
func guiState(env Env) string {
	if env.goos() != "darwin" {
		return stateUnsupported
	}
	if env.DryRun {
		return stateAvailable // DryRun (Setup) never calls Run beyond its own install-state probes
	}
	_, err := env.run("osascript", "-e", `tell application "System Events" to get name of first process`)
	if err == nil {
		return stateAvailable
	}
	msg := err.Error()
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		msg = string(ee.Stderr)
	}
	if strings.Contains(msg, "-1719") || strings.Contains(msg, "-25211") {
		return stateDisabled
	}
	return stateAvailable // some other failure (e.g. osascript missing) — never claim disabled without the specific error
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
