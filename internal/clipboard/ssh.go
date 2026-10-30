package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net"
	"os"
	"os/user"
	"runtime"
	"time"
)

// sshTimeout bounds one bridge round trip: the Mac side runs osascript per paste (~0.3–1s).
const sshTimeout = 10 * time.Second

// sshMaxPNG caps what the bridge may send, so a misbehaving peer cannot exhaust memory.
const sshMaxPNG = 64 << 20

// errNoBridge: inside an SSH session with no forwarded socket — never fall back to the host clipboard.
var errNoBridge = errors.New("no clipboard bridge from the SSH client")

// errNoSSHImage: the bridge answered with zero bytes, the protocol's "no image".
var errNoSSHImage = errors.New("no image in the SSH client's clipboard — copy one there first (or its bridge is down)")

// inSSH reports whether this process runs inside an SSH login, where the clipboard is the client's.
func inSSH() bool { return os.Getenv("SSH_CONNECTION") != "" }

// sshSocket is where the client's `RemoteForward /tmp/claude-clip-%r.sock` lands.
func sshSocket() string {
	name := os.Getenv("USER")
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	return "/tmp/claude-clip-" + name + ".sock"
}

// ReadSSH returns the SSH client's clipboard PNG through the forwarded bridge socket.
func ReadSSH() ([]byte, error) { return readSSH(sshSocket()) }

// readSSH dials sock and reads one PNG; the server writes it and closes, zero bytes meaning none.
func readSSH(sock string) ([]byte, error) {
	if err := checkSocket(sock); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: sshTimeout}
	conn, err := d.DialContext(context.Background(), "unix", sock)
	if err != nil {
		return nil, fmt.Errorf("%w (%s: %v)", errNoBridge, sock, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(sshTimeout))
	b, err := io.ReadAll(io.LimitReader(conn, sshMaxPNG+1))
	if err != nil {
		return nil, fmt.Errorf("clipboard bridge read: %w", err)
	}
	switch {
	case len(b) == 0:
		return nil, errNoSSHImage
	case len(b) > sshMaxPNG:
		return nil, fmt.Errorf("clipboard bridge sent over %d MiB", sshMaxPNG>>20)
	}
	if _, err := png.DecodeConfig(bytes.NewReader(b)); err != nil {
		return nil, fmt.Errorf("clipboard bridge sent a non-PNG: %w", err)
	}
	return b, nil
}

// checkSocket refuses a /tmp path another local user could have pre-bound: it must be our own socket.
func checkSocket(sock string) error {
	fi, err := os.Lstat(sock)
	switch {
	case err != nil:
		return fmt.Errorf("%w (%s: add RemoteForward to the client's ~/.ssh/config)", errNoBridge, sock)
	case fi.Mode()&os.ModeSocket == 0:
		return fmt.Errorf("%w (%s is not a socket)", errNoBridge, sock)
	}
	if uid, ok := ownerUID(fi); !ok || uid != os.Getuid() {
		return fmt.Errorf("%w (%s is not owned by you)", errNoBridge, sock)
	}
	return nil
}

// grabSSH saves the SSH client's clipboard image as a local PNG and returns its path plus "WxH".
func grabSSH() (path, dim string, err error) {
	b, err := ReadSSH()
	if err != nil {
		return "", "", err
	}
	return savePNG(b)
}

// ServePNG writes the macOS clipboard PNG to w for one bridge connection; no image writes nothing.
func ServePNG(w io.Writer) error {
	if runtime.GOOS != "darwin" {
		return errors.New("clip --serve runs on macOS only (the SSH client side)")
	}
	return servePNG(w)
}

// servePNG is ServePNG without the darwin gate, for tests with a faked osascript.
func servePNG(w io.Writer) error {
	path, _, err := grabMac()
	if errors.Is(err, errNoMacImage) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}
