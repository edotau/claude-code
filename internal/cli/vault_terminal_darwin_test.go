package cli

import (
	"os"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ptyIoctl is the raw TIOCPTY* ioctl darwin has no golang.org/x/sys/unix wrapper for (unlike Linux's
// IoctlGetInt/IoctlSetPointerInt); arg is a pointer for TIOCPTYGNAME, 0 for TIOCPTYGRANT/TIOCPTYUNLK.
func ptyIoctl(fd uintptr, req uint, arg uintptr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(req), arg)
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
	if err := ptyIoctl(master.Fd(), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&nameBuf[0]))); err != nil {
		t.Fatal(err)
	}
	if err := ptyIoctl(master.Fd(), unix.TIOCPTYGRANT, 0); err != nil {
		t.Fatal(err)
	}
	if err := ptyIoctl(master.Fd(), unix.TIOCPTYUNLK, 0); err != nil {
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
