package clipboard

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeWlPaste installs a wl-paste script as the only PATH entry; body runs with $1 = first arg.
func fakeWlPaste(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "wl-paste"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return dir
}

// bitfieldsBMP builds a 2x1 32bpp BI_BITFIELDS BMP (40-byte header + trailing masks), the shape WSLg emits.
func bitfieldsBMP() []byte {
	b := make([]byte, 14+40+12+8)
	copy(b, "BM")
	binary.LittleEndian.PutUint32(b[2:], uint32(len(b)))
	binary.LittleEndian.PutUint32(b[10:], 14+40+12)
	binary.LittleEndian.PutUint32(b[14:], 40)
	binary.LittleEndian.PutUint32(b[18:], 2) // width
	binary.LittleEndian.PutUint32(b[22:], 1) // height
	binary.LittleEndian.PutUint16(b[26:], 1)
	binary.LittleEndian.PutUint16(b[28:], 32)
	binary.LittleEndian.PutUint32(b[30:], biBitFields)
	binary.LittleEndian.PutUint32(b[54:], 0x00FF0000)
	binary.LittleEndian.PutUint32(b[58:], 0x0000FF00)
	binary.LittleEndian.PutUint32(b[62:], 0x000000FF)
	binary.LittleEndian.PutUint32(b[66:], 0x00FF0000)
	binary.LittleEndian.PutUint32(b[70:], 0x000000FF)
	return b
}

func TestGrabWSLgNoImageTypes(t *testing.T) {
	fakeWlPaste(t, `[ "$1" = "--list-types" ] && { echo text/plain; exit 0; }; exit 9`)
	if _, _, err := grabWSLg(); !errors.Is(err, errNoImage) {
		t.Fatalf("err = %v, want errNoImage", err)
	}
}

func TestGrabWSLgBMPToPNG(t *testing.T) {
	dir := fakeWlPaste(t, `[ "$1" = "--list-types" ] && { echo image/bmp; exit 0; }; cat "$(dirname "$0")/img.bmp"`)
	if err := os.WriteFile(filepath.Join(dir, "img.bmp"), bitfieldsBMP(), 0o644); err != nil {
		t.Fatal(err)
	}
	path, dim, err := grabWSLg()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if dim != "2x1" {
		t.Errorf("dim = %q, want 2x1", dim)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(raw), "\x89PNG") {
		t.Errorf("not a PNG file at %s (%v)", path, err)
	}
}

func TestWlPasteReachesWSLgSocketWithoutWaylandDisplay(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env")
	fakeWlPaste(t, `echo "$WAYLAND_DISPLAY $CLAUDE_WL_PASTE_PROXY" > `+out)
	t.Setenv("WAYLAND_DISPLAY", "")
	if _, err := wlPaste("--list-types"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if want := wslgSocket + " 0"; strings.TrimSpace(string(got)) != want {
		t.Errorf("child env = %q, want %q", got, want)
	}
	t.Setenv("WAYLAND_DISPLAY", "wayland-1")
	if _, err := wlPaste("--list-types"); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "wayland-1" {
		t.Errorf("set WAYLAND_DISPLAY was overridden: %q", got)
	}
}

func TestGrabBusyIsLockedError(t *testing.T) {
	_, _, err := grab(func() bool { return true },
		func(string) (string, error) { return "BUSY\n", nil },
		func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("err = %v, want locked error", err)
	}
	if errors.Is(err, errNoImage) {
		t.Error("BUSY must not read as no-image")
	}
}

// pngBytes is a 3x2 PNG.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func fakeOsascript(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "osascript"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
}

func TestGrabMac(t *testing.T) {
	src := filepath.Join(t.TempDir(), "img.png")
	if err := os.WriteFile(src, pngBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeOsascript(t, `for a; do last="$a"; done; cp '`+src+`' "$last"; echo OK`)
	path, dim, err := grabMac()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if dim != "3x2" {
		t.Errorf("dim = %q, want 3x2", dim)
	}

	fakeOsascript(t, `for a; do last="$a"; done; echo "$last" > '`+src+`.path'; echo NOIMAGE`)
	if _, _, err := grabMac(); err == nil || !strings.Contains(err.Error(), "no image") {
		t.Fatalf("err = %v, want no-image", err)
	}
	p, _ := os.ReadFile(src + ".path")
	if _, err := os.Stat(strings.TrimSpace(string(p))); !os.IsNotExist(err) {
		t.Errorf("temp file left behind: %s", p)
	}
}
