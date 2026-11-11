package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// spec is one known browser's per-platform identity: detection, profile and host dirs, and default-handler match.
type spec struct {
	name        string // registry key, also Browser.Name
	displayName string
	family      string

	darwinBundleID   string // matched case-insensitively against LSHandlerRoleAll and mdfind
	darwinSupportDir string // relative to ~/Library/Application Support

	linuxBinary      string // "command -v" target; "" = not supported on Linux
	linuxConfigDir   string // relative to ~/.config
	linuxDesktopFile string // matched against `xdg-settings get default-web-browser`
}

// knownBrowsers is every browser this package can detect, in the order Detect reports them.
var knownBrowsers = []spec{
	{
		name: "chrome", displayName: "Google Chrome", family: familyChromium,
		darwinBundleID: "com.google.Chrome", darwinSupportDir: "Google/Chrome",
		linuxBinary: "google-chrome", linuxConfigDir: "google-chrome", linuxDesktopFile: "google-chrome.desktop",
	},
	{
		name: "edge", displayName: "Microsoft Edge", family: familyChromium,
		darwinBundleID: "com.microsoft.edgemac", darwinSupportDir: "Microsoft Edge",
		linuxBinary: "microsoft-edge", linuxConfigDir: "microsoft-edge", linuxDesktopFile: "microsoft-edge.desktop",
	},
	{
		name: "brave", displayName: "Brave", family: familyChromium,
		darwinBundleID: "com.brave.Browser", darwinSupportDir: "BraveSoftware/Brave-Browser",
		linuxBinary: "brave-browser", linuxConfigDir: "BraveSoftware/Brave-Browser",
		linuxDesktopFile: "brave-browser.desktop",
	},
	{
		name: "arc", displayName: "Arc", family: familyChromium,
		darwinBundleID: "company.thebrowser.Browser", darwinSupportDir: "Arc/User Data",
		// no Linux build.
	},
	{
		name: "vivaldi", displayName: "Vivaldi", family: familyChromium,
		darwinBundleID: "com.vivaldi.Vivaldi", darwinSupportDir: "Vivaldi",
		linuxBinary: "vivaldi", linuxConfigDir: "vivaldi", linuxDesktopFile: "vivaldi-stable.desktop",
	},
	{
		name: "chromium", displayName: "Chromium", family: familyChromium,
		darwinBundleID: "org.chromium.Chromium", darwinSupportDir: "Chromium",
		linuxBinary: "chromium", linuxConfigDir: "chromium", linuxDesktopFile: "chromium.desktop",
	},
	{
		name: "safari", displayName: "Safari", family: familySafari,
		darwinBundleID: "com.apple.Safari",
		// macOS only; no NativeMessagingHosts-style profile tree this package reads.
	},
	{
		name: "firefox", displayName: "Firefox", family: familyFirefox,
		darwinBundleID: "org.mozilla.firefox",
		linuxBinary:    "firefox", linuxDesktopFile: "firefox.desktop",
	},
}

func specByName(name string) (spec, bool) {
	for _, s := range knownBrowsers {
		if s.name == name {
			return s, true
		}
	}
	return spec{}, false
}

// specByBundleID matches LaunchServices' LSHandlerRoleAll, which macOS stores lowercased.
func specByBundleID(id string) (spec, bool) {
	for _, s := range knownBrowsers {
		if s.darwinBundleID != "" && strings.EqualFold(s.darwinBundleID, id) {
			return s, true
		}
	}
	return spec{}, false
}

func specByDesktopFile(f string) (spec, bool) {
	for _, s := range knownBrowsers {
		if s.linuxDesktopFile != "" && strings.EqualFold(s.linuxDesktopFile, f) {
			return s, true
		}
	}
	return spec{}, false
}

// errDefaultUnsupported marks a system default with no extension/cdp path (Safari or Firefox, tier 3).
var errDefaultUnsupported = errors.New(
	"browser: system default has no extension/cdp control path (tier 3: safaridriver/geckodriver)")

// Default resolves the https handler to a known browser; the Browser is filled even when the error says unsupported.
func Default(env Env) (Browser, error) {
	switch env.goos() {
	case "darwin":
		return defaultDarwin(env)
	case "linux":
		if env.WSL {
			return defaultWSL(env)
		}
		return defaultLinux(env)
	default:
		return Browser{}, fmt.Errorf("browser: default-browser detection is unsupported on %s", env.goos())
	}
}

type lsHandler struct {
	LSHandlerURLScheme string `json:"LSHandlerURLScheme"`
	LSHandlerRoleAll   string `json:"LSHandlerRoleAll"`
}

// defaultDarwin reads LaunchServices' secure plist (via plutil -convert json) for the https handler.
func defaultDarwin(env Env) (Browser, error) {
	path := filepath.Join(env.Home, "Library", "Preferences", "com.apple.LaunchServices",
		"com.apple.launchservices.secure.plist")
	out, err := env.run("plutil", "-convert", "json", "-o", "-", path)
	if err != nil {
		return Browser{}, fmt.Errorf("browser: reading LSHandlers: %w", err)
	}
	var doc struct {
		LSHandlers []lsHandler `json:"LSHandlers"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return Browser{}, fmt.Errorf("browser: parsing LSHandlers: %w", err)
	}
	for _, h := range doc.LSHandlers {
		if h.LSHandlerURLScheme == "https" {
			s, ok := specByBundleID(h.LSHandlerRoleAll)
			return resolveDefault(env, s, ok, h.LSHandlerRoleAll)
		}
	}
	return Browser{}, errors.New("browser: no https LSHandler found")
}

// defaultLinux reads `xdg-settings get default-web-browser` (a .desktop file name).
func defaultLinux(env Env) (Browser, error) {
	out, err := env.run("xdg-settings", "get", "default-web-browser")
	if err != nil {
		return Browser{}, fmt.Errorf("browser: xdg-settings: %w", err)
	}
	desktop := strings.TrimSpace(out)
	s, ok := specByDesktopFile(desktop)
	return resolveDefault(env, s, ok, desktop)
}

// resolveDefault detects the matched spec; a non-Chromium result is errDefaultUnsupported but keeps its identity.
func resolveDefault(env Env, s spec, ok bool, raw string) (Browser, error) {
	if !ok {
		return Browser{}, fmt.Errorf("browser: system default %q is not a recognized browser: %w", raw, errDefaultUnsupported)
	}
	b := detectOne(env, s)
	b.Default = true
	if s.family != familyChromium {
		return b, fmt.Errorf("browser: system default is %s (%s): %w", b.DisplayName, b.Family, errDefaultUnsupported)
	}
	return b, nil
}
