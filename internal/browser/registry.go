package browser

import "strings"

// spec is one known browser's platform-specific identity: how to detect it, where its profiles and
// native-messaging host live, and how to recognize it as the resolved system default.
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
		linuxBinary: "brave-browser", linuxConfigDir: "BraveSoftware/Brave-Browser", linuxDesktopFile: "brave-browser.desktop",
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
