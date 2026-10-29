// Package clipboard works around WSL2 image paste: it transcodes WSLg's bridged BMP into a decodable
// BMP/PNG, saves the Windows clipboard image via PowerShell, and installs the wl-paste proxy.
package clipboard

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/pathview"
)

// clipPS saves the Windows clipboard image as a PNG (GDI+, bypassing WSLg's 32bpp BI_BITFIELDS BMP
// that common decoders reject). Prints "OK <winpath> <W>x<H>" or "NOIMAGE". -Sta: MTA lacks the API.
const clipPS = `Add-Type -AssemblyName System.Windows.Forms,System.Drawing
if (-not [System.Windows.Forms.Clipboard]::ContainsImage()) { Write-Output 'NOIMAGE'; exit 0 }
$img = [System.Windows.Forms.Clipboard]::GetImage()
$p = Join-Path $env:TEMP ('claude-clip-' + [Guid]::NewGuid().ToString('N').Substring(0,8) + '.png')
$img.Save($p, [System.Drawing.Imaging.ImageFormat]::Png)
Write-Output ('OK ' + $p + ' ' + $img.Width + 'x' + $img.Height)`

// clipOKRE parses the success line. The path is matched lazily up to the trailing WxH so a Windows
// temp dir containing spaces ("C:\Users\First Last\...") survives intact.
var clipOKRE = regexp.MustCompile(`^OK (.+?) (\d+x\d+)$`)

// powershellTimeout bounds the Windows round trip so hung interop fails instead of stalling the verb.
const powershellTimeout = 30 * time.Second

// runPowerShell executes a PowerShell script on the Windows side and returns its stdout.
func runPowerShell(script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), powershellTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Sta", "-Command", script).Output()
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(out), "\r", ""), nil
}

// Grab saves the Windows clipboard image to a PNG and returns its WSL path plus "WxH".
func Grab() (path, dim string, err error) {
	return grab(hooks.OnWSL, runPowerShell, translateWin)
}

// translateWin maps a Windows path to its WSL view, "" when no mount covers it.
func translateWin(win string) string {
	p, _ := pathview.Resolve(win)
	return p
}

// grab is Grab with WSL detection, PowerShell and path translation injected for tests off Windows.
func grab(onWSL func() bool, run func(string) (string, error), translate func(string) string) (path, dim string, err error) {
	if !onWSL() {
		return "", "", errors.New("clipboard image grab needs WSL — the Windows clipboard is only reachable through powershell.exe")
	}
	out, err := run(clipPS)
	if err != nil {
		return "", "", fmt.Errorf("powershell clipboard read failed: %w", err)
	}
	line := strings.TrimSpace(out)
	if line == "NOIMAGE" {
		return "", "", errors.New("no image in the Windows clipboard — copy or screenshot one first (Win+Shift+S)")
	}
	m := clipOKRE.FindStringSubmatch(line)
	if m == nil {
		return "", "", fmt.Errorf("unexpected powershell output: %q", line)
	}
	wsl := translate(m[1])
	if wsl == "" {
		return "", "", fmt.Errorf("could not map %q into WSL", m[1])
	}
	return wsl, m[2], nil
}
