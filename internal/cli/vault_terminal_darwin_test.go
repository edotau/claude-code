package cli

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

func TestVaultGetKeyRefusesTTY(t *testing.T) {
	useVaultTest(t)
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	var nameBuf [128]byte
	if err := ptyIoctl(master.Fd(), unix.TIOCPTYGNAME, unsafe.Pointer(&nameBuf[0])); err != nil {
		t.Fatal(err)
	}
	if err := ptyIoctl(master.Fd(), unix.TIOCPTYGRANT, nil); err != nil {
		t.Fatal(err)
	}
	if err := ptyIoctl(master.Fd(), unix.TIOCPTYUNLK, nil); err != nil {
		t.Fatal(err)
	}
	name := string(nameBuf[:])
	if i := strings.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	slave, err := os.OpenFile(name, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	code, stdout, stderr := captureOutput(t, func() int {
		old := os.Stdout
		os.Stdout = slave
		defer func() { os.Stdout = old }()
		return cmdVault([]string{"get", "--key"})
	})
	if code != 2 || stdout != "" || !strings.Contains(stderr, "refusing") {
		t.Fatalf("get --key on TTY: exit %d, stdout empty=%v, refusal=%v",
			code, stdout == "", strings.Contains(stderr, "refusing"))
	}
}
