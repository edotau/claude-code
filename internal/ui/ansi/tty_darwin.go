//go:build darwin

package ansi

import "syscall"

// ttyGetReq is the termios get request for isTTY.
const ttyGetReq = syscall.TIOCGETA
