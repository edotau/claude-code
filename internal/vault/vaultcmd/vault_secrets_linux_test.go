package vaultcmd

import (
	"fmt"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"

	"golang.org/x/sys/unix"
)

func TestVaultGetKeyRefusesTTY(t *testing.T) {
	vaulttest.UseFreshKey(t)
	_, slave := vaultTestPTY(t)
	assertGetKeyRefusesTTY(t, slave)
}

func vaultTestPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}
