//go:build darwin

package proc

import "syscall"

const ioctlGet = syscall.TIOCGETA
const ioctlSet = syscall.TIOCSETA
