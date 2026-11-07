//go:build linux || darwin

package tui

import (
	"errors"
	"io"
	"syscall"
	"unsafe"
)

// termState is the termios snapshot makeRaw returns so restore can undo it.
type termState struct{ t syscall.Termios }

func ioctlTermios(fd int, req uint, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}

// isTerminal reports whether fd is a character device the termios ioctls accept.
func isTerminal(fd int) bool {
	var t syscall.Termios
	return ioctlTermios(fd, ioctlGet, &t) == nil
}

// makeRaw mirrors cfmakeraw(3): no echo, no canonical mode, no signals or flow control, 1-byte reads.
func makeRaw(fd int) (*termState, error) {
	var t syscall.Termios
	if err := ioctlTermios(fd, ioctlGet, &t); err != nil {
		return nil, err
	}
	prev := &termState{t: t}
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN], t.Cc[syscall.VTIME] = 1, 0
	if err := ioctlTermios(fd, ioctlSet, &t); err != nil {
		return nil, err
	}
	return prev, nil
}

// restore puts fd back to the state makeRaw captured.
func restore(fd int, st *termState) error {
	if st == nil {
		return nil
	}
	return ioctlTermios(fd, ioctlSet, &st.t)
}

// Size is the terminal's (columns, rows) from TIOCGWINSZ.
func Size(fd int) (int, int, error) {
	var ws struct{ Row, Col, X, Y uint16 }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws))); e != 0 {
		return 0, 0, e
	}
	return int(ws.Col), int(ws.Row), nil
}

// errCanceled is what readPassword returns when Esc, Ctrl-C or Ctrl-D ends the read before Enter.
var errCanceled = errors.New("tui: input canceled")

// readPassword reads one line from fd with echo off, calling onChange (if set) after every read so a paste shows
// as it lands; Backspace and Ctrl-U edit, Esc/Ctrl-C/Ctrl-D cancel. The terminal is restored before returning.
func readPassword(fd int, onChange func([]byte)) ([]byte, error) {
	var t syscall.Termios
	if err := ioctlTermios(fd, ioctlGet, &t); err != nil {
		return nil, err
	}
	prev := t
	t.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG
	t.Iflag |= syscall.ICRNL
	t.Cc[syscall.VMIN], t.Cc[syscall.VTIME] = 1, 0
	if err := ioctlTermios(fd, ioctlSet, &t); err != nil {
		return nil, err
	}
	defer func() { _ = ioctlTermios(fd, ioctlSet, &prev) }()
	var line []byte
	var esc escState
	buf := make([]byte, 4096)
	for {
		n, err := readFD(fd, buf)
		var done bool
		var kerr error
		if line, done, kerr = feedPassword(line, buf[:n], &esc); done {
			return line, kerr
		}
		if esc == escSeen && !waitReadable(fd, pollTick) {
			return nil, errCanceled // a lone Esc, not the start of a sequence split across reads
		}
		if onChange != nil && n > 0 {
			onChange(line)
		}
		if err != nil {
			return line, err
		}
	}
}

// feedPassword applies one read's bytes to line, skipping escape sequences; done once Enter or a cancel ends it.
func feedPassword(line, in []byte, esc *escState) (_ []byte, done bool, err error) {
	for _, c := range in {
		if esc.skip(c) {
			continue
		}
		l, done, kerr := editLine(line, c)
		if line = l; done {
			return line, true, kerr
		}
	}
	return line, false, nil
}

// editLine applies one typed byte to line; done is true on Enter (err nil) or Ctrl-C/Ctrl-D (errCanceled).
func editLine(line []byte, c byte) (_ []byte, done bool, err error) {
	switch {
	case c == '\n' || c == '\r':
		return line, true, nil
	case c == 0x03 || c == 0x04:
		return nil, true, errCanceled
	case c == 0x7f || c == 0x08:
		return line[:max(0, len(line)-1)], false, nil
	case c == 0x15:
		return line[:0], false, nil
	case c >= 0x20:
		return append(line, c), false, nil
	}
	return line, false, nil
}

// readFD reads fd with read(2) directly: an os.NewFile wrapper is closed by its finalizer, which would close stdin.
func readFD(fd int, buf []byte) (int, error) {
	for {
		n, err := syscall.Read(fd, buf)
		switch {
		case err == syscall.EINTR:
			continue
		case err != nil:
			return 0, err
		case n == 0:
			return 0, io.EOF
		}
		return n, nil
	}
}

// escState tracks an ANSI escape sequence across reads so a bracketed-paste marker (ESC[200~, ESC[201~) or an
// arrow key is dropped instead of leaking "[200~" into the secret.
type escState int

const (
	escNone escState = iota
	escSeen
	escCSI
	escSS3
)

// skip reports whether c belongs to an escape sequence and is to be ignored.
func (s *escState) skip(c byte) bool {
	switch *s {
	case escSeen:
		*s = escNone
		if c == '[' {
			*s = escCSI
			return true
		}
		if c == 'O' {
			*s = escSS3
			return true
		}
	case escSS3: // ESC O x: application-mode arrow or function key, one final byte
		*s = escNone
		return true
	case escCSI:
		if c >= 0x40 && c <= 0x7e {
			*s = escNone
		}
		return true
	default:
		if c == 0x1b {
			*s = escSeen
			return true
		}
	}
	return false
}
