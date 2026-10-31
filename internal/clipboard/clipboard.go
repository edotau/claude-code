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
	"runtime"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
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

// Grab saves the clipboard image to a PNG and returns its path plus "WxH": macOS osascript; WSLg, then PowerShell.
func Grab() (path, dim string, err error) {
	if inSSH() {
		return grabSSH() // the keyboard's machine owns the clipboard; never read the host's
	}
	if runtime.GOOS == "darwin" {
		return grabMac()
	}
	if proc.OnWSL() {
		if path, dim, err = grabWSLg(); err == nil || errors.Is(err, errNoImage) {
			return path, dim, err
		}
	}
	return grab(proc.OnWSL, runPowerShell, translateWin)
}

// wslgSocket is WSLg's Wayland socket; a shell without WSLg's env (VS Code Remote-WSL) still reaches it.
const wslgSocket = "/mnt/wslg/runtime-dir/wayland-0"

// errNoImage: the clipboard answered and holds no image, so there is nothing for PowerShell to find either.
var errNoImage = errors.New("no image in the Windows clipboard — copy or screenshot one first (Win+Shift+S)")

// wlPaste runs wl-paste against WSLg, reaching the socket by path when the shell lacks WAYLAND_DISPLAY.
func wlPaste(args ...string) ([]byte, error) {
	env := os.Environ()
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		env = append(env, "WAYLAND_DISPLAY="+wslgSocket, "CLAUDE_WL_PASTE_PROXY=0")
	}
	return captureWlPaste("wl-paste", env, args)
}

// captureWlPaste runs the wl-paste binary bin with env under a 10s timeout and returns its stdout.
func captureWlPaste(bin string, env, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
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
	return savePNG(buf.Bytes())
}

// savePNG writes PNG bytes to a temp file and returns its path plus "WxH".
func savePNG(b []byte) (path, dim string, err error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return "", "", err
	}
	f, err := os.CreateTemp("", "claude-clip-*.png")
	if err != nil {
		return "", "", err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return "", "", err
	}
	if err := f.Close(); err != nil {
		return "", "", err
	}
	return f.Name(), fmt.Sprintf("%dx%d", cfg.Width, cfg.Height), nil
}

// errNoMacImage: osascript found no PNG on the macOS clipboard.
var errNoMacImage = errors.New("no image in the clipboard — copy or screenshot one first (Cmd+Ctrl+Shift+4)")

// macClipScript writes the clipboard's PNG to argv 1; "NOIMAGE" when the clipboard holds none.
const macClipScript = `on run argv
	try
		set d to the clipboard as «class PNGf»
	on error
		return "NOIMAGE"
	end try
	set fh to open for access (POSIX file (item 1 of argv)) with write permission
	set eof fh to 0
	write d to fh
	close access fh
	return "OK"
end run`

// grabMac saves the macOS clipboard image as a PNG through osascript and returns its path plus "WxH".
func grabMac() (path, dim string, err error) {
	f, err := os.CreateTemp("", "claude-clip-*.png")
	if err != nil {
		return "", "", err
	}
	path = f.Name()
	_ = f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", macClipScript, path).Output()
	if err != nil {
		_ = os.Remove(path)
		return "", "", fmt.Errorf("osascript clipboard read failed: %w", err)
	}
	if strings.TrimSpace(string(out)) != "OK" {
		_ = os.Remove(path)
		return "", "", errNoMacImage
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		_ = os.Remove(path)
		return "", "", fmt.Errorf("clipboard PNG unreadable: %w", err)
	}
	return path, fmt.Sprintf("%dx%d", cfg.Width, cfg.Height), nil
}

// translateWin maps a Windows path to its WSL view, "" when no mount covers it.
func translateWin(win string) string {
	p, _ := paths.Resolve(win)
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
