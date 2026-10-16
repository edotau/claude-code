//go:build linux || darwin

package tui

import (
	"os"
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

// getSize is the terminal's (columns, rows) from TIOCGWINSZ.
func getSize(fd int) (int, int, error) {
	var ws struct{ Row, Col, X, Y uint16 }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws))); e != 0 {
		return 0, 0, e
	}
	return int(ws.Col), int(ws.Row), nil
}

// readPassword reads one line from fd with echo off, restoring the terminal before returning.
func readPassword(fd int) ([]byte, error) {
	var t syscall.Termios
	if err := ioctlTermios(fd, ioctlGet, &t); err != nil {
		return nil, err
	}
	prev := t
	t.Lflag &^= syscall.ECHO
	t.Lflag |= syscall.ICANON | syscall.ISIG
	t.Iflag |= syscall.ICRNL
	if err := ioctlTermios(fd, ioctlSet, &t); err != nil {
		return nil, err
	}
	defer func() { _ = ioctlTermios(fd, ioctlSet, &prev) }()
	f := os.NewFile(uintptr(fd), "stdin")
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := f.Read(buf)
		if n == 1 {
			if buf[0] == '\n' || buf[0] == '\r' {
				break
			}
			line = append(line, buf[0])
		}
		if err != nil {
			return line, err
		}
	}
	return line, nil
}
