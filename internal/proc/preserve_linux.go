//go:build linux

package proc

import "syscall"

const ioctlGet = syscall.TCGETS
const ioctlSet = syscall.TCSETS
