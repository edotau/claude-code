package tui

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func testPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	var unlock, number uint32
	testIoctl(t, master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock))
	testIoctl(t, master, syscall.TIOCGPTN, unsafe.Pointer(&number))
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

func testIoctl(t *testing.T, f *os.File, req uintptr, value unsafe.Pointer) {
	t.Helper()
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(value)); err != 0 {
		t.Fatal(err)
	}
}

func testPickerTTY(t *testing.T, height uint16, input string) {
	t.Helper()
	master, slave := testPTY(t)
	size := [4]uint16{height, 80, 0, 0}
	testIoctl(t, slave, syscall.TIOCSWINSZ, unsafe.Pointer(&size))
	out, err := os.CreateTemp(t.TempDir(), "picker-output")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	stdin, stdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = slave, out
	t.Cleanup(func() { os.Stdin, os.Stdout = stdin, stdout })
	if _, err := master.WriteString(input); err != nil {
		t.Fatal(err)
	}
}

func TestPickEmptyOnTwoLineTerminal(t *testing.T) {
	testPickerTTY(t, 2, "q")
	if idx, err := Pick("", nil, 0); idx != -1 || err != nil {
		t.Fatalf("empty Pick on 80x2 = %d, %v; want -1, nil", idx, err)
	}
}

func TestPickClampsStartPastLastRow(t *testing.T) {
	testPickerTTY(t, 24, "\n")
	rows := [][2]string{{"a", ""}, {"b", ""}, {"c", ""}}
	if idx, err := Pick("", rows, 5); idx != 2 || err != nil {
		t.Fatalf("Pick(start=5) = %d, %v; want last row 2, nil", idx, err)
	}
}

func TestTerminalReportsInputEIO(t *testing.T) {
	master, slave := testPTY(t)
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}
	term := Terminal{fd: int(master.Fd()), in: master, keys: make(chan Key, 8),
		stop: make(chan struct{}), done: make(chan struct{})}
	go term.readKeys()
	t.Cleanup(term.stopReader)
	select {
	case _, ok := <-term.Keys():
		if ok {
			t.Fatal("input EIO emitted a key instead of closing the stream")
		}
	case <-time.After(time.Second):
		t.Fatal("input EIO did not close the key stream")
	}
	if err := term.Err(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("Err after Keys closed = %v; want input EIO", err)
	}
}
