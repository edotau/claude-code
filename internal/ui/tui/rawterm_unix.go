//go:build linux || darwin

package tui

import (
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
	defer restoreOnSignal(func() { _ = ioctlTermios(fd, ioctlSet, &prev) })()
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
