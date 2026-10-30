package browser

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/pathview"
)

// CDPPort is the remote-debugging port the WSL path starts a Windows browser on.
const CDPPort = 9222

// devtoolsMCP is the browser MCP Claude Code gets on WSL (pinned: a moving @latest is a supply-chain hole).
var devtoolsMCP = []string{"npx", "-y", "chrome-devtools-mcp@1.11.0", "--browserUrl", "http://localhost:" + strconv.Itoa(CDPPort)}

// devtoolsMCPName is the MCP server name registered in Claude Code's user scope.
const devtoolsMCPName = "chrome-devtools"

// stateUnsupportedWSL marks the extension path on WSL: Claude in Chrome is documented as unsupported there.
const stateUnsupportedWSL = "unsupported-wsl"

// winExe is each browser's Windows executable under the C: drive, first match wins.
var winExe = map[string][]string{
	"edge":    {"Program Files (x86)/Microsoft/Edge/Application/msedge.exe", "Program Files/Microsoft/Edge/Application/msedge.exe"},
	"chrome":  {"Program Files/Google/Chrome/Application/chrome.exe", "Program Files (x86)/Google/Chrome/Application/chrome.exe"},
	"brave":   {"Program Files/BraveSoftware/Brave-Browser/Application/brave.exe"},
	"vivaldi": {"Program Files/Vivaldi/Application/vivaldi.exe"},
	"firefox": {"Program Files/Mozilla Firefox/firefox.exe"},
}

// winProgID maps the Windows https UserChoice ProgId prefix to a browser.
var winProgID = map[string]string{"MSEdgeHTM": "edge", "ChromeHTML": "chrome", "BraveHTML": "brave", "VivaldiHTM": "vivaldi", "FirefoxURL": "firefox"}

// cdpBrowserTag is the substring of /json/version's "Browser" field that names who owns the port.
var cdpBrowserTag = map[string]string{"edge": "Edg/", "chrome": "Chrome/", "brave": "Chrome/", "vivaldi": "Chrome/"}

// isWSL reports a WSL distro: the env var WSL sets, or its interop binfmt entry.
func isWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	_, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop")
	return err == nil
}

// winExePath is the browser's Windows executable as seen from WSL, or "".
func winExePath(env Env, name string) string {
	for _, rel := range winExe[name] {
		p := filepath.Join(env.WinRoot, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// detectWindows fills a browser installed on the Windows host: the extension path is unsupported from WSL,
// and the CDP path is ready when this browser already answers on CDPPort, else available.
func detectWindows(env Env, s spec, b *Browser) {
	b.Installed = true
	b.Host = stateUnsupportedWSL
	b.Paths = Paths{Extension: stateUnsupportedWSL, WebDriver: stateUnsupported, GUI: stateUnsupported, CDP: stateUnsupported}
	if s.family != familyChromium {
		return
	}
	b.Paths.CDP = stateAvailable
	if owner := env.cdpOwner(); owner != "" && strings.Contains(owner, cdpBrowserTag[s.name]) {
		b.Paths.CDP = stateReady
	}
}

// cdpOwner is the "Browser" field of http://localhost:CDPPort/json/version, or "" when nothing answers.
func (e Env) cdpOwner() string {
	if e.Probe != nil {
		return e.Probe()
	}
	return ""
}

// probeCDP is the real Env.Probe: one GET with a short timeout.
func probeCDP() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c := http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:"+strconv.Itoa(CDPPort)+"/json/version", nil)
	if err != nil {
		return ""
	}
	resp, err := c.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var v struct {
		Browser string `json:"Browser"`
	}
	if decodeJSON(resp.Body, &v) != nil {
		return ""
	}
	return v.Browser
}

// defaultWSL reads the Windows https UserChoice ProgId through PowerShell interop.
func defaultWSL(env Env) (Browser, error) {
	out, err := env.run("powershell.exe", "-NoProfile", "-Command",
		`(Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https\UserChoice').ProgId`)
	if err != nil {
		return Browser{}, fmt.Errorf("browser: reading the Windows default browser: %w", err)
	}
	progID := strings.TrimSpace(out)
	for prefix, name := range winProgID {
		if strings.HasPrefix(progID, prefix) {
			s, ok := specByName(name)
			return resolveDefault(env, s, ok, progID)
		}
	}
	return resolveDefault(env, spec{}, false, progID)
}

// setupWSL starts the Windows browser with CDP on a dedicated profile when nothing answers yet, then registers
// chrome-devtools-mcp in Claude Code so sessions in WSL can drive it.
func setupWSL(env Env, b Browser) Actions {
	acts := Actions{Browser: b.Name}
	acts.Items = append(acts.Items, Action{Kind: "noop", Executed: true,
		Description: "Claude in Chrome is unsupported from WSL (code.claude.com/docs/en/chrome); using CDP instead."})
	acts.Items = append(acts.Items, startCDPAction(env, b))
	acts.Items = append(acts.Items, registerMCPAction(env))
	return acts
}

// startCDPAction launches the browser via PowerShell Start-Process with the debug port and a dedicated
// profile (%LOCALAPPDATA%\claude-code\<name>-cdp), then waits for the port to answer from WSL.
func startCDPAction(env Env, b Browser) Action {
	if strings.Contains(env.cdpOwner(), cdpBrowserTag[b.Name]) {
		return Action{Kind: "noop", Executed: true, Description: fmt.Sprintf("%s already answers on localhost:%d.", b.DisplayName, CDPPort)}
	}
	exe := winExePath(env, b.Name)
	drive := pathview.NewResolver([]pathview.Mount{{Point: env.WinRoot, Kind: pathview.KindDrvFS, Drive: "C"}})
	winExe, _ := drive.ViewFrom(exe, pathview.Target{Style: pathview.StyleWindows})
	ps := fmt.Sprintf(`Start-Process -FilePath '%s' -ArgumentList '--remote-debugging-port=%d',`+
		`"--user-data-dir=$env:LOCALAPPDATA\claude-code\%s-cdp",'--no-first-run','--no-default-browser-check'`,
		winExe, CDPPort, b.Name)
	a := Action{Kind: "start-cdp", Command: []string{"powershell.exe", "-NoProfile", "-Command", ps},
		Description: fmt.Sprintf("Starting %s with --remote-debugging-port=%d on a dedicated profile.", b.DisplayName, CDPPort)}
	if env.DryRun {
		return a
	}
	if out, err := env.run(a.Command[0], a.Command[1:]...); err != nil {
		a.Error = strings.TrimSpace(out + " " + err.Error())
		return a
	}
	for range 20 {
		if strings.Contains(env.cdpOwner(), cdpBrowserTag[b.Name]) {
			a.Executed = true
			return a
		}
		env.sleep(500 * time.Millisecond)
	}
	a.Error = cdpFailure(env, b)
	return a
}

// cdpFailure explains a dead CDP port: stale processes on the profile first, then a missing mirrored networking.
func cdpFailure(env Env, b Browser) string {
	msg := fmt.Sprintf("started, but localhost:%d does not answer from WSL", CDPPort)
	if pids := staleProfilePIDs(env, b.Name); len(pids) > 0 {
		return fmt.Sprintf("%s; %s process(es) %s already own the %s-cdp profile and ignored the debug flag: run "+
			"powershell.exe -NoProfile -Command \"Stop-Process -Id %s\", then rerun setup", msg, b.DisplayName,
			strings.Join(pids, ", "), b.Name, strings.Join(pids, ","))
	}
	if !wslMirrored(env) {
		return msg + "; .wslconfig lacks networkingMode=mirrored under [wsl2]"
	}
	return msg + "; check that no other " + b.DisplayName + " window owns the profile"
}

// staleProfilePIDs lists msedge-style processes whose command line names the <name>-cdp profile.
func staleProfilePIDs(env Env, name string) []string {
	exe := filepath.Base(winExe[name][0])
	ps := fmt.Sprintf(`Get-CimInstance Win32_Process -Filter "Name='%s'" | Where-Object { $_.CommandLine -like '*%s-cdp*' } `+
		`| ForEach-Object { $_.ProcessId }`, exe, name)
	out, err := env.run("powershell.exe", "-NoProfile", "-Command", ps)
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// wslMirrored reports networkingMode=mirrored under [wsl2] in any Windows user's .wslconfig.
func wslMirrored(env Env) bool {
	files, _ := filepath.Glob(filepath.Join(env.WinRoot, "Users", "*", ".wslconfig"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err == nil && mirroredIn(string(data)) {
			return true
		}
	}
	return false
}

// mirroredIn scans .wslconfig text for networkingMode=mirrored inside [wsl2], case-insensitively.
func mirroredIn(text string) bool {
	inWSL2 := false
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(line, "[") {
			inWSL2 = line == "[wsl2]"
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && inWSL2 && strings.TrimSpace(k) == "networkingmode" && strings.TrimSpace(strings.SplitN(v, "#", 2)[0]) == "mirrored" {
			return true
		}
	}
	return false
}

// registerMCPAction adds chrome-devtools-mcp at user scope unless ~/.claude.json already has it.
func registerMCPAction(env Env) Action {
	cmd := append([]string{env.claudeBin(), "mcp", "add", "-s", "user", devtoolsMCPName, "--"}, devtoolsMCP...)
	a := Action{Kind: "register-mcp", Command: cmd,
		Description: "Registering chrome-devtools-mcp in Claude Code (user scope) against localhost:" + strconv.Itoa(CDPPort) + "."}
	if mcpRegistered(env.Home) {
		return Action{Kind: "noop", Executed: true, Description: devtoolsMCPName + " MCP is already registered in Claude Code."}
	}
	if !env.HasNPX {
		a.Error = "npx not found: install Node.js 20+ (the MCP runs via npx), then rerun claude-code network browser --setup"
		return a
	}
	if env.DryRun {
		return a
	}
	out, err := env.run(cmd[0], cmd[1:]...)
	a.Executed = err == nil
	if err != nil {
		a.Error = strings.TrimSpace(out + " " + err.Error())
	}
	return a
}

// mcpRegistered reports a chrome-devtools entry in ~/.claude.json's user-scope mcpServers.
func mcpRegistered(home string) bool {
	f, err := os.Open(filepath.Join(home, ".claude.json"))
	if err != nil {
		return false
	}
	defer f.Close()
	var cfg struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if decodeJSON(f, &cfg) != nil {
		return false
	}
	_, ok := cfg.MCPServers[devtoolsMCPName]
	return ok
}
