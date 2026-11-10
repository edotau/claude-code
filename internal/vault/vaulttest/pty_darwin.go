package vaulttest

import (
	"os"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ptyIoctl is the raw TIOCPTY* ioctl darwin has no golang.org/x/sys/unix wrapper for (unlike Linux's
// IoctlGetInt/IoctlSetPointerInt); arg is a pointer for TIOCPTYGNAME, nil for TIOCPTYGRANT/TIOCPTYUNLK.
// The Pointer->uintptr conversion stays inline in the Syscall call so the compiler keeps arg alive
// until the call completes; a stack-moved buffer would otherwise invalidate the address first.
func ptyIoctl(fd uintptr, req uint, arg unsafe.Pointer) error {
	//lint:ignore SA1019 darwin has no libSystem wrapper for a buffer-filling ioctl
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(req), uintptr(arg)) //nolint:staticcheck // see lint:ignore above
	if errno != 0 {
		return errno
	}
	return nil
}

// PTY opens a pseudo-terminal pair closed at cleanup; the slave reads as a terminal.
func PTY(t testing.TB) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close() })
	var nameBuf [128]byte
	if err := ptyIoctl(master.Fd(), unix.TIOCPTYGNAME, unsafe.Pointer(&nameBuf[0])); err != nil {
		t.Fatal(err)
	}
	for _, req := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err := ptyIoctl(master.Fd(), req, nil); err != nil {
			t.Fatal(err)
		}
	}
	name := string(nameBuf[:])
	if i := strings.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	slave, err = os.OpenFile(name, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}
