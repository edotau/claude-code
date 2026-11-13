// Package browser finds, per installed browser, its control path (extension, CDP, WebDriver, GUI) and the OS default.
package browser

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// errNoRunner is returned by Env.run when a caller built an Env without setting Run.
var errNoRunner = errors.New("browser: no command runner configured on Env")

// extensionID is Claude Code's Chrome extension id; the host manifest and profile Extensions/<id> dirs key off it.
const extensionID = "fcoeoabgfenejglbffodgkkbkcdhcgfn"

// hostManifestName is the native-messaging host manifest Claude Code registers; this package only reads it.
const hostManifestName = "com.anthropic.claude_code_browser_extension.json"

// webStoreURL is the Chrome Web Store page for the extension, opened (never fetched) by Setup.
const webStoreURL = "https://chromewebstore.google.com/detail/" + extensionID

// Family values on browser.Family.
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

// browser is one known browser's full detection result.
type browser struct {
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

// Report is the full `network browser` result.
type Report struct {
	Browsers    []browser `json:"browsers"`
	Default     string    `json:"default"`                // the default browser's Name, "" when unresolved
	DefaultErr  string    `json:"defaultError,omitempty"` // why default detection failed (not "unsupported default")
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
	cdpListen  func() string       // Detect's memoized cdpListener, so N browsers share one PowerShell probe
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

// envGOOS and onWSL are NewEnv's platform probes; tests swap them to build every platform's Env on any host.
var (
	envGOOS = runtime.GOOS
	onWSL   = proc.OnWSL
)

// NewEnv builds the real Env with actual paths, bounded commands, and time.Now.
func NewEnv() Env {
	home, _ := os.UserHomeDir()
	return Env{
		Home:       home,
		AppSupport: appSupportRoot(home, envGOOS),
		Run:        runReal,
		ProbeRun:   runProbe,
		Now:        time.Now,
		GOOS:       envGOOS,
		WSL:        envGOOS == "linux" && onWSL(),
		WinRoot:    paths.WinRootFrom(paths.Default()),
		Probe:      probeCDP,
		Sleep:      time.Sleep,
		ClaudeBin:  realClaude(),
		HasNPX:     onPath("npx"),
	}
}

// appSupportRoot is macOS's "~/Library/Application Support" or Linux's "~/.config".
func appSupportRoot(home, goos string) string {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support")
	}
	return filepath.Join(home, ".config")
}

// commandTimeout bounds one detection command. 30s covers a slow setup probe; tests set it lower.
var commandTimeout = 30 * time.Second

// SetCommandTimeout overrides commandTimeout for a test and returns the restore; production keeps 30s.
func SetCommandTimeout(d time.Duration) (restore func()) {
	saved := commandTimeout
	commandTimeout = d
	return func() { commandTimeout = saved }
}

// runReal bounds detection/setup commands and returns trimmed stdout.
func runReal(name string, args ...string) (string, error) {
	return runCommand(commandTimeout, name, args...)
}

// guiProbeTimeout bounds the System Events probe: it answers in ~0.1s, and a hung one must not stall detection.
var guiProbeTimeout = 5 * time.Second

// runProbe is runReal under guiProbeTimeout.
func runProbe(name string, args ...string) (string, error) {
	return runCommand(guiProbeTimeout, name, args...)
}

func runCommand(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if name == "powershell.exe" && !onPath(name) { // interop left Windows off PATH (VS Code Remote-WSL terminals)
		name = paths.WinPowerShell()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// realClaude resolves the agent claude binary, or "" on error.
func realClaude() string {
	p, _ := paths.AgentBin("claude")
	return p
}

// onPath reports whether name resolves on PATH.
func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
