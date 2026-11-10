// Package browser finds, per installed browser, its control path (extension, CDP, WebDriver, GUI) and the OS default.
package browser

import (
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// errNoRunner is returned by Env.run when a caller built an Env without setting Run.
var errNoRunner = errors.New("browser: no command runner configured on Env")

// extensionID is Claude Code's Chrome extension id; the host manifest and profile Extensions/<id> dirs key off it.
const extensionID = "fcoeoabgfenejglbffodgkkbkcdhcgfn"

// hostManifestName is the native-messaging host manifest Claude Code registers; this package only reads it.
const hostManifestName = "com.anthropic.claude_code_browser_extension.json"

// webStoreURL is the Chrome Web Store page for the extension, opened (never fetched) by Setup.
const webStoreURL = "https://chromewebstore.google.com/detail/" + extensionID

// Family values on Browser.Family.
const (
	familyChromium = "chromium"
	familySafari   = "safari"
	familyFirefox  = "firefox"
)

// State values shared across Host and every Paths field.
const (
	stateReady        = "ready"
	stateNoExtension  = "no-extension"
	stateNoHost       = "no-host"
	stateNotInstalled = "not-installed"
	stateUnsupported  = "unsupported"
	stateDisabled     = "disabled"
	stateAvailable    = "available"
)

// hostManifest is the native-messaging host manifest's on-disk shape; only the fields this package reads.
type hostManifest struct {
	Path           string   `json:"path"`
	AllowedOrigins []string `json:"allowed_origins"`
}

// profile is one browser profile's extension-install state.
type profile struct {
	Name      string `json:"name"`
	Extension string `json:"extension"` // ready|disabled|no-extension
}

// controlPaths holds one state per control path; every browser gets all four, "unsupported" where its family can't.
type controlPaths struct {
	Extension string `json:"extension"` // ready|no-extension|no-host|not-installed|unsupported
	CDP       string `json:"cdp"`       // available|not-installed|unsupported
	WebDriver string `json:"webdriver"` // available|disabled|not-installed|unsupported
	GUI       string `json:"gui"`       // available|not-installed|unsupported
}

// Browser is one known browser's full detection result.
type Browser struct {
	Name        string       `json:"name"` // chrome|edge|brave|arc|vivaldi|chromium|safari|firefox
	DisplayName string       `json:"displayName"`
	Family      string       `json:"family"` // chromium|safari|firefox
	Installed   bool         `json:"installed"`
	BundleID    string       `json:"bundleId,omitempty"`
	Default     bool         `json:"default,omitempty"` // this browser is the resolved system default
	Host        string       `json:"host"`              // host manifest: ready|no-host|unsupported|not-installed
	Profiles    []profile    `json:"profiles,omitempty"`
	Paths       controlPaths `json:"paths"`
}

// Report is the full `web browsers` result.
type Report struct {
	Browsers    []Browser `json:"browsers"`
	Default     string    `json:"default"` // the default browser's Name, "" when unresolved
	GeneratedAt time.Time `json:"generatedAt"`
}

// Env is the seam: real roots and command runner in production, fixtures in tests; DryRun makes Setup only describe.
type Env struct {
	Home       string
	AppSupport string // macOS: ~/Library/Application Support; Linux: ~/.config
	Run        func(name string, args ...string) (string, error)
	ProbeRun   func(name string, args ...string) (string, error) // short-bounded GUI probe; nil falls back to Run
	Now        func() time.Time
	GOOS       string
	DryRun     bool
	WSL        bool                // a WSL distro: browsers live on the Windows host under WinRoot
	WinRoot    string              // the C: drive as seen from WSL (/mnt/c)
	Probe      func() string       // CDP /json/version "Browser" on localhost:CDPPort, "" when nothing answers
	Sleep      func(time.Duration) // wait seam for the CDP start poll
	ClaudeBin  string              // the real claude binary for `claude mcp add` (never our shim)
	HasNPX     bool                // npx is on PATH: chrome-devtools-mcp runs through it
	gui        func() string       // Detect's memoized guiState, so N browsers share one osascript probe
}

// sleep is e.Sleep when set, else a no-op (tests never wait).
func (e Env) sleep(d time.Duration) {
	if e.Sleep != nil {
		e.Sleep(d)
	}
}

// claudeBin is e.ClaudeBin, else plain "claude" from PATH.
func (e Env) claudeBin() string {
	if e.ClaudeBin != "" {
		return e.ClaudeBin
	}
	return "claude"
}

// decodeJSON decodes one JSON value from r into v.
func decodeJSON(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

// goos is e.GOOS when set (tests pin it to exercise macOS logic off-machine), else runtime.GOOS.
func (e Env) goos() string {
	if e.GOOS != "" {
		return e.GOOS
	}
	return runtime.GOOS
}

// now is e.Now() when set, else time.Now — a clock seam so Report.GeneratedAt is deterministic in tests.
func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// run calls e.Run, reporting a clear error instead of panicking when a caller forgot to set it.
func (e Env) run(name string, args ...string) (string, error) {
	if e.Run == nil {
		return "", errNoRunner
	}
	return e.Run(name, args...)
}

// probeRun calls e.ProbeRun when set, else e.run.
func (e Env) probeRun(name string, args ...string) (string, error) {
	if e.ProbeRun != nil {
		return e.ProbeRun(name, args...)
	}
	return e.run(name, args...)
}

// guiState is Detect's memoized probe when set, else a fresh one (detectOne called outside Detect).
func (e Env) guiState() string {
	if e.gui != nil {
		return e.gui()
	}
	return guiState(e)
}

// supportDir is spec's Application-Support-relative directory for goos ("" when unsupported there).
func (s spec) supportDir(goos string) string {
	if goos == "darwin" {
		return s.darwinSupportDir
	}
	return s.linuxConfigDir
}

// appSupportPath joins env.AppSupport with s's support dir for goos.
func appSupportPath(env Env, s spec) string {
	return filepath.Join(env.AppSupport, s.supportDir(env.goos()))
}

// orDash renders "" as "-" for text output.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
