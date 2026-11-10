package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

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
