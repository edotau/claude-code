// Package clipboard works around WSL2 image paste: it transcodes WSLg's bridged BMP into a decodable
// BMP/PNG, saves the Windows clipboard image via PowerShell, and installs the wl-paste proxy.
package clipboard

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math/bits"
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

// WSLg hands a Windows clipboard image to Wayland as 32bpp BI_BITFIELDS, rejected by x/image/bmp (upstream #50552).

// maxBMPBytes bounds the total input read — a clipboard screenshot has no business exceeding this.
const maxBMPBytes = 128 << 20

// maxPixels bounds width*height before any pixel buffer is allocated, ahead of the per-dimension
// check below — two dimensions each under the cap can still multiply into a DoS-sized allocation.
const maxPixels = 64 << 20

// maxDim is a per-dimension sanity ceiling — generous past any real screenshot, cheap overflow guard.
const maxDim = 1 << 16

const (
	fileHeaderSize = 14
	biRGB          = 0
	biBitFields    = 3
)

// raster is a decoded image as packed 24bpp BGR rows, each padded to stride. topDown reports that
// rows[0] is the TOP image row; a plain BMP stores the bottom row first.
type raster struct {
	width, height, stride int
	rows                  []byte
	topDown               bool
}

// Transcode reads a BMP from r and writes a widely-decodable BMP to w: 24bpp, BI_RGB, bottom-up,
// rows padded to 4 bytes.
func Transcode(r io.Reader, w io.Writer) error {
	img, err := decodeRaster(r)
	if err != nil {
		return err
	}
	return writeBMP24(w, img.width, img.height, img.stride, img.rows, img.topDown)
}

// TranscodePNG reads a BMP from r and writes a PNG to w, so the CLI's `wl-paste --type image/png`
// leg can be answered on a WSLg clipboard that only ever offers BMP.
func TranscodePNG(r io.Reader, w io.Writer) error {
	img, err := decodeRaster(r)
	if err != nil {
		return err
	}
	return writePNG(w, img)
}

// decodeRaster reads a BMP from r and returns its pixels as packed 24bpp BGR rows. Shared by both
// public encoders so the format-specific part is only the write. Alpha is discarded — correct for a
// paste target, not a bug.
func decodeRaster(r io.Reader) (raster, error) {
	data, dib, pixelOffset, err := readBMPHeader(r)
	if err != nil {
		return raster{}, err
	}
	img, err := bmpDimensions(dib)
	if err != nil {
		return raster{}, err
	}
	bitCount := binary.LittleEndian.Uint16(dib[14:16])
	compression := binary.LittleEndian.Uint32(dib[16:20])
	rMask, gMask, bMask, err := bmpColorMasks(data, dib, bitCount, compression)
	if err != nil {
		return raster{}, err
	}

	img.stride = rowStride(img.width, int(bitCount))
	pixelBytes := img.stride * img.height
	if int64(pixelOffset)+int64(pixelBytes) > int64(len(data)) {
		return raster{}, fmt.Errorf("clipboard: truncated pixel data (want %d bytes at offset %d, have %d)", pixelBytes, pixelOffset, len(data))
	}
	img.rows = data[pixelOffset : int(pixelOffset)+pixelBytes]

	// BI_RGB 24bpp already matches the target shape — pass the bytes through, only fix orientation.
	if compression == biRGB && bitCount == 24 {
		return img, nil
	}
	return convertRaster(img, bitCount, rMask, gMask, bMask), nil
}

func readBMPHeader(r io.Reader) (data, dib []byte, pixelOffset uint32, err error) {
	data, err = io.ReadAll(io.LimitReader(r, maxBMPBytes+1))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("clipboard: read input: %w", err)
	}
	if len(data) > maxBMPBytes {
		return nil, nil, 0, fmt.Errorf("clipboard: input exceeds %d byte cap", maxBMPBytes)
	}
	if len(data) < fileHeaderSize+4 {
		return nil, nil, 0, fmt.Errorf("clipboard: truncated file header (%d bytes)", len(data))
	}
	if data[0] != 'B' || data[1] != 'M' {
		return nil, nil, 0, fmt.Errorf("clipboard: not a BMP (magic %q)", data[0:2])
	}
	pixelOffset = binary.LittleEndian.Uint32(data[10:14])

	dibSize := binary.LittleEndian.Uint32(data[14:18])
	var headerLen int
	switch dibSize {
	case 40, 108, 124: // BITMAPINFOHEADER, BITMAPV4HEADER, BITMAPV5HEADER
		headerLen = int(dibSize)
	default:
		return nil, nil, 0, fmt.Errorf("clipboard: unsupported DIB header size %d", dibSize)
	}
	if len(data) < fileHeaderSize+headerLen {
		return nil, nil, 0, fmt.Errorf("clipboard: truncated DIB header (want %d bytes, have %d)", headerLen, len(data)-fileHeaderSize)
	}
	return data, data[fileHeaderSize : fileHeaderSize+headerLen], pixelOffset, nil
}

func bmpDimensions(dib []byte) (raster, error) {
	width := int32(binary.LittleEndian.Uint32(dib[4:8]))
	height := int32(binary.LittleEndian.Uint32(dib[8:12]))

	if width <= 0 {
		return raster{}, fmt.Errorf("clipboard: invalid width %d", width)
	}
	topDown := height < 0 // negative biHeight means top-down row order
	if topDown {
		height = -height
	}
	if height <= 0 {
		// Catches height==0 and MinInt32, whose negation overflows back to itself.
		return raster{}, fmt.Errorf("clipboard: invalid height %d", height)
	}
	if width > maxDim || int32(height) > maxDim {
		return raster{}, fmt.Errorf("clipboard: dimensions %dx%d exceed the %d cap", width, height, maxDim)
	}
	if int64(width)*int64(height) > maxPixels {
		return raster{}, fmt.Errorf("clipboard: %dx%d (%d pixels) exceeds the %d pixel cap", width, height, int64(width)*int64(height), maxPixels)
	}
	return raster{width: int(width), height: int(height), topDown: topDown}, nil
}

func bmpColorMasks(data, dib []byte, bitCount uint16, compression uint32) (rMask, gMask, bMask uint32, err error) {
	switch compression {
	case biRGB:
		if bitCount != 24 && bitCount != 32 {
			return 0, 0, 0, fmt.Errorf("clipboard: unsupported bit depth %d for BI_RGB", bitCount)
		}
		if bitCount == 32 {
			rMask, gMask, bMask = 0x00FF0000, 0x0000FF00, 0x000000FF
		}
	case biBitFields:
		if bitCount != 16 && bitCount != 32 {
			return 0, 0, 0, fmt.Errorf("clipboard: unsupported bit depth %d for BI_BITFIELDS", bitCount)
		}
		if len(dib) == 40 {
			// classic extension: masks trail the 40-byte header, ahead of the pixel array.
			maskOff := fileHeaderSize + 40
			if len(data) < maskOff+12 {
				return 0, 0, 0, errors.New("clipboard: truncated BITFIELDS masks")
			}
			rMask = binary.LittleEndian.Uint32(data[maskOff : maskOff+4])
			gMask = binary.LittleEndian.Uint32(data[maskOff+4 : maskOff+8])
			bMask = binary.LittleEndian.Uint32(data[maskOff+8 : maskOff+12])
		} else {
			// V4/V5: masks live inline in the DIB header at fixed offsets.
			rMask = binary.LittleEndian.Uint32(dib[40:44])
			gMask = binary.LittleEndian.Uint32(dib[44:48])
			bMask = binary.LittleEndian.Uint32(dib[48:52])
		}
	default:
		return 0, 0, 0, fmt.Errorf("clipboard: unsupported compression %d (RLE/JPEG/PNG-in-BMP not handled)", compression)
	}
	return rMask, gMask, bMask, nil
}

func convertRaster(img raster, bitCount uint16, rMask, gMask, bMask uint32) raster {
	dstStride := rowStride(img.width, 24)
	out := make([]byte, dstStride*img.height)
	rShift, rWidth := maskShiftWidth(rMask)
	gShift, gWidth := maskShiftWidth(gMask)
	bShift, bWidth := maskShiftWidth(bMask)
	bytesPerPixel := int(bitCount) / 8

	for y := 0; y < img.height; y++ {
		srcRow := img.rows[y*img.stride : y*img.stride+img.stride]
		dstRow := out[y*dstStride : y*dstStride+dstStride]
		for x := 0; x < img.width; x++ {
			off := x * bytesPerPixel
			var px uint32
			switch bytesPerPixel {
			case 2:
				px = uint32(binary.LittleEndian.Uint16(srcRow[off : off+2]))
			case 4:
				px = binary.LittleEndian.Uint32(srcRow[off : off+4])
			}
			dstRow[x*3+0] = scaleChannel(px, bMask, bShift, bWidth)
			dstRow[x*3+1] = scaleChannel(px, gMask, gShift, gWidth)
			dstRow[x*3+2] = scaleChannel(px, rMask, rShift, rWidth)
		}
	}
	img.stride, img.rows = dstStride, out
	return img
}

// rowStride returns the 4-byte-padded row length for width pixels at bitCount depth.
func rowStride(width, bitCount int) int {
	return ((width*bitCount + 31) / 32) * 4
}

// maskShiftWidth returns the bit offset and population count of a channel mask (0,0 if mask is 0).
// Masks in real BMPs are contiguous, so OnesCount doubles as the run's bit width.
func maskShiftWidth(mask uint32) (shift, width int) {
	if mask == 0 {
		return 0, 0
	}
	return bits.TrailingZeros32(mask), bits.OnesCount32(mask)
}

// scaleChannel extracts and rescales a channel to 8 bits — narrower masks (e.g. 5-bit 555) are
// scaled up proportionally rather than truncated, so black stays black and white stays white.
func scaleChannel(px, mask uint32, shift, width int) byte {
	if mask == 0 || width == 0 {
		return 0
	}
	val := (px & mask) >> shift
	maxVal := uint32(1)<<width - 1
	return byte(val * 255 / maxVal)
}

// writeBMP24 emits a BITMAPFILEHEADER + 40-byte BITMAPINFOHEADER, 24bpp BI_RGB, bottom-up. rows is
// in on-disk row order for the source (row 0 = file's first row); reversed here iff that was top-down.
func writeBMP24(w io.Writer, width, height, stride int, rows []byte, topDown bool) error {
	dataSize := stride * height
	fileSize := fileHeaderSize + 40 + dataSize

	var fh [fileHeaderSize]byte
	fh[0], fh[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(fh[2:6], uint32(fileSize))
	binary.LittleEndian.PutUint32(fh[10:14], fileHeaderSize+40)
	if _, err := w.Write(fh[:]); err != nil {
		return fmt.Errorf("clipboard: write file header: %w", err)
	}

	var dib [40]byte
	binary.LittleEndian.PutUint32(dib[0:4], 40)
	binary.LittleEndian.PutUint32(dib[4:8], uint32(width))
	binary.LittleEndian.PutUint32(dib[8:12], uint32(height)) // positive: bottom-up
	binary.LittleEndian.PutUint16(dib[12:14], 1)             // planes
	binary.LittleEndian.PutUint16(dib[14:16], 24)            // bitcount
	binary.LittleEndian.PutUint32(dib[16:20], biRGB)
	binary.LittleEndian.PutUint32(dib[20:24], uint32(dataSize))
	if _, err := w.Write(dib[:]); err != nil {
		return fmt.Errorf("clipboard: write DIB header: %w", err)
	}

	if !topDown {
		if _, err := w.Write(rows); err != nil {
			return fmt.Errorf("clipboard: write pixel data: %w", err)
		}
		return nil
	}
	for y := height - 1; y >= 0; y-- {
		if _, err := w.Write(rows[y*stride : (y+1)*stride]); err != nil {
			return fmt.Errorf("clipboard: write pixel data: %w", err)
		}
	}
	return nil
}

// writePNG encodes a decoded raster as PNG. BGR→RGB per pixel, and a bottom-up source is read in
// reverse so the result is never upside down.
func writePNG(w io.Writer, img raster) error {
	out := image.NewRGBA(image.Rect(0, 0, img.width, img.height))
	for y := 0; y < img.height; y++ {
		src := y
		if !img.topDown {
			src = img.height - 1 - y // bottom-up: file row 0 is the last image row
		}
		row := img.rows[src*img.stride : src*img.stride+img.stride]
		dst := out.Pix[y*out.Stride : (y+1)*out.Stride]
		for x := 0; x < img.width; x++ {
			dst[x*4+0] = row[x*3+2] // R
			dst[x*4+1] = row[x*3+1] // G
			dst[x*4+2] = row[x*3+0] // B
			dst[x*4+3] = 0xFF       // BMP alpha is discarded, so the PNG is fully opaque
		}
	}
	if err := png.Encode(w, out); err != nil {
		return fmt.Errorf("clipboard: encode png: %w", err)
	}
	return nil
}
