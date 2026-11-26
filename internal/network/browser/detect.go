package browser

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
)

// memoizeProbes shares one PowerShell listener probe across every browser checked.
func memoizeProbes(env Env) Env {
	env.cdpListen = sync.OnceValue(func() string { return cdpListener(env) })
	return env
}

// Detect probes every known browser plus the system default and returns one report.
func Detect(env Env) Report {
	env = memoizeProbes(env)
	browsers := make([]browser, 0, len(knownBrowsers))
	for _, s := range knownBrowsers {
		b := detectOne(env, s)
		b.Fix = fixFor(b)
		browsers = append(browsers, b)
	}
	def, err := Default(env) // errDefaultUnsupported still carries the resolved (non-Chromium) name
	if def.Name != "" {
		for i := range browsers {
			browsers[i].Default = browsers[i].Name == def.Name
		}
	}
	r := Report{Browsers: browsers, Default: def.Name, GeneratedAt: env.now()}
	if def.Name == "" && failedDefault(env, err) {
		r.DefaultErr = err.Error()
	}
	return r
}

// DetectDefault reports only the system default browser; the error is a failed detection, not an unsupported default.
func DetectDefault(env Env) (Report, error) {
	env = memoizeProbes(env)
	def, err := Default(env) // errDefaultUnsupported still carries the resolved (non-Chromium) name
	if def.Name == "" {
		if failedDefault(env, err) {
			return Report{GeneratedAt: env.now()}, err
		}
		return Report{GeneratedAt: env.now()}, nil
	}
	return Report{Browsers: []browser{def}, Default: def.Name, GeneratedAt: env.now()}, nil
}

// failedDefault reports a real detection failure; other OSes have no detector and an unsupported default is no failure.
func failedDefault(env Env, err error) bool {
	return err != nil && !errors.Is(err, errDefaultUnsupported) && (env.goos() == "darwin" || env.goos() == "linux")
}

// detectOne builds one browser's full result: identity, install state, then its control paths.
func detectOne(env Env, s spec) browser {
	b := browser{Name: s.name, DisplayName: s.displayName, Family: s.family}
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
	detectFamily(env, s, &b)
	return b
}

// detectFamily fills the host and control paths for an installed browser by its family.
func detectFamily(env Env, s spec, b *browser) {
	switch s.family {
	case familyChromium:
		detectChromium(env, s, b)
	case familySafari:
		detectSafari(env, b)
	case familyFirefox:
		detectFirefox(env, b)
	}
}

func stubUnsupported(b browser) browser {
	b.Host = stateUnsupported
	b.Paths = controlPaths{Extension: stateUnsupported, CDP: stateUnsupported}
	return b
}

func stubNotInstalled(b browser) browser {
	b.Host = stateNotInstalled
	b.Paths = controlPaths{Extension: stateNotInstalled, CDP: stateNotInstalled}
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

// detectChromium fills in a Chromium-family browser's host, profiles, and the control paths.
func detectChromium(env Env, s spec, b *browser) {
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
}

// checkHost reports whether the NativeMessagingHosts manifest exists, its target exists and its origin matches.
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
	return slices.Contains(m.AllowedOrigins, want)
}

// detectProfiles lists supportDir/Default and supportDir/Profile * and the extension state in each.
func detectProfiles(supportDir string) []profile {
	var names []string
	if paths.IsDir(filepath.Join(supportDir, "Default")) {
		names = append(names, "Default")
	}
	if matches, err := filepath.Glob(filepath.Join(supportDir, "Profile *")); err == nil {
		for _, m := range matches {
			if paths.IsDir(m) {
				names = append(names, filepath.Base(m))
			}
		}
	}
	slices.Sort(names)
	profiles := make([]profile, 0, len(names))
	for _, name := range names {
		profiles = append(profiles, profile{Name: name, Extension: profileExtensionState(filepath.Join(supportDir, name))})
	}
	return profiles
}

// profileExtensionState: the extension dir must exist under this profile; Preferences can still mark it disabled.
func profileExtensionState(profileDir string) string {
	if !paths.IsDir(filepath.Join(profileDir, "Extensions", extensionID)) {
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

// rollupExtension is the browser-level extension state: ready if any profile is, else disabled, else no-extension.
func rollupExtension(profiles []profile) string {
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

// detectSafari: Safari has no Claude extension and no CDP; WebDriver-based control is not built.
func detectSafari(_ Env, b *browser) {
	b.Host = stateUnsupported
	b.Paths = controlPaths{Extension: stateUnsupported, CDP: stateUnsupported}
}

// detectFirefox: Firefox has no Claude extension and no CDP here; WebDriver-based control is not built.
func detectFirefox(_ Env, b *browser) {
	b.Host = stateUnsupported
	b.Paths = controlPaths{Extension: stateUnsupported, CDP: stateUnsupported}
}
