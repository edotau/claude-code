// Package clipboard works around WSL2 image paste: it transcodes WSLg's bridged BMP into a decodable
// BMP/PNG, saves the Windows clipboard image via PowerShell, and installs the wl-paste proxy.
package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/pathview"
)

// clipPS saves the Windows clipboard image as a PNG (GDI+, bypassing WSLg's 32bpp BI_BITFIELDS BMP
// that common decoders reject). Prints "OK <winpath> <W>x<H>", "NOIMAGE" or "BUSY". -Sta: MTA lacks the API.
const clipPS = `Add-Type -AssemblyName System.Windows.Forms,System.Drawing
$img = $null
for ($i = 0; $i -lt 10; $i++) { # another app holding the clipboard makes every call throw; retry briefly
  try { if (-not [System.Windows.Forms.Clipboard]::ContainsImage()) { Write-Output 'NOIMAGE'; exit 0 }
        $img = [System.Windows.Forms.Clipboard]::GetImage(); if ($img) { break } } catch {}
  Start-Sleep -Milliseconds 200
}
if (-not $img) { Write-Output 'BUSY'; exit 0 }
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
	ps := "powershell.exe"
	if _, err := exec.LookPath(ps); err != nil {
		ps = wslPowerShell // interop left Windows off PATH (VS Code Remote-WSL terminals)
	}
	out, err := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-Sta", "-Command", script).Output()
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(out), "\r", ""), nil
}

// Grab saves the Windows clipboard image to a PNG and returns its WSL path plus "WxH": WSLg first, then PowerShell.
func Grab() (path, dim string, err error) {
	if hooks.OnWSL() {
		if path, dim, err = grabWSLg(); err == nil || errors.Is(err, errNoImage) {
			return path, dim, err
		}
	}
	return grab(hooks.OnWSL, runPowerShell, translateWin)
}

// wslgSocket is WSLg's Wayland socket; a shell without WSLg's env (VS Code Remote-WSL) still reaches it.
const wslgSocket = "/mnt/wslg/runtime-dir/wayland-0"

// errNoImage: the clipboard answered and holds no image, so there is nothing for PowerShell to find either.
var errNoImage = errors.New("no image in the Windows clipboard — copy or screenshot one first (Win+Shift+S)")

// wlPaste runs wl-paste against WSLg, reaching the socket by path when the shell lacks WAYLAND_DISPLAY.
func wlPaste(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wl-paste", args...)
	cmd.Env = os.Environ()
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		cmd.Env = append(cmd.Env, "WAYLAND_DISPLAY="+wslgSocket, "CLAUDE_WL_PASTE_PROXY=0")
	}
	return cmd.Output()
}

// grabWSLg reads WSLg's bridged BMP through wl-paste and saves it as a local PNG — no Windows round trip.
func grabWSLg() (path, dim string, err error) {
	if types, err := wlPaste("--list-types"); err == nil && !bytes.Contains(types, []byte("image/")) {
		return "", "", errNoImage
	}
	bmp, err := wlPaste("--type", "image/bmp")
	if err != nil || len(bmp) == 0 {
		return "", "", fmt.Errorf("wl-paste image/bmp: %v", err)
	}
	var buf bytes.Buffer
	if err := TranscodePNG(bytes.NewReader(bmp), &buf); err != nil {
		return "", "", err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return "", "", err
	}
	f, err := os.CreateTemp("", "claude-clip-*.png")
	if err != nil {
		return "", "", err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return "", "", err
	}
	if err := f.Close(); err != nil {
		return "", "", err
	}
	return f.Name(), fmt.Sprintf("%dx%d", cfg.Width, cfg.Height), nil
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
	if line == "BUSY" {
		return "", "", errors.New("the Windows clipboard is locked by another app — try again")
	}
	if line == "NOIMAGE" {
		return "", "", errNoImage
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
