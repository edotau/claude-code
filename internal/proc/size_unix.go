//go:build linux || darwin

package proc

import "golang.org/x/sys/unix"

// Size is the terminal's (columns, rows) from TIOCGWINSZ.
func Size(fd int) (int, int, error) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}
