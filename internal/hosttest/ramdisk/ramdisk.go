// Package ramdisk runs a command with TMPDIR on a throwaway APFS RAM disk: test temp-dir churn queues on the macOS
// Data volume (full suite 67s -> 27s), and go test's own build files land there too (peak 394 MiB).
package ramdisk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

const (
	offEnv  = "TEST_RAMDISK"
	sectors = 8388608 // 4 GiB in 512-byte sectors; RAM is only used as files are written

	exitUsage    = 2
	exitNotFound = 127
)

// Package vars so the stalled-call test can shrink them.
var (
	diskTimeout   = 2 * time.Minute // per hdiutil/diskutil call; a stalled one must not hang the run
	diskWaitDelay = 10 * time.Second
)

// page is the help for `claude-code test ramdisk`.
var page = helptext.Page{
	Usage: "claude-code test ramdisk [--] <command> [args...]",
	About: "Run <command> with TMPDIR on a throwaway 4 GiB APFS RAM disk, then remove it.\n" +
		"Off macOS, or if the disk cannot be made, it runs on the normal TMPDIR.",
	Hint: "TEST_RAMDISK=0 skips the disk. The exit code is <command>'s.",
}

// Seams: tests never mount a real disk.
var (
	goos   = runtime.GOOS
	attach = attachDisk
	detach = detachDisk
)

// Run is the `test ramdisk` entry; it returns the command's exit code.
func Run(args []string) int {
	fs := helptext.NewFlagSet("test ramdisk", page)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "claude-code: test ramdisk: need a <command>")
		return exitUsage
	}
	if goos != "darwin" || os.Getenv(offEnv) == "0" {
		return run(argv, "")
	}
	dev, tmp, err := attach(fmt.Sprintf("claude-code-test-%d", os.Getpid()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: test ramdisk: using the normal TMPDIR (expect a slower run): %v\n", err)
		return run(argv, "")
	}
	defer detach(dev)
	return run(argv, tmp)
}

// run starts argv with TMPDIR set when tmp is non-empty and survives signals, so the caller lives to detach.
func run(argv []string, tmp string) int {
	env := os.Environ()
	if tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}
	code, err := proc.RunForwarding(argv[0], argv[1:], env, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: test ramdisk: %v\n", err)
		return exitNotFound
	}
	if code < 0 {
		return 1 // killed by a signal, which has no exit code of its own
	}
	return code
}

// diskCmd bounds a disk-management command by diskTimeout; the caller must call cancel once it has run.
func diskCmd(name string, args ...string) (cmd *exec.Cmd, cancel context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), diskTimeout)
	cmd = exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = diskWaitDelay // a child holding the output pipe open must not block past the deadline
	return cmd, cancel
}

// attachDisk makes the RAM disk and returns its device and a TMPDIR on it.
func attachDisk(volume string) (dev, tmp string, err error) {
	cmd, cancel := diskCmd("hdiutil", "attach", "-nomount", fmt.Sprintf("ram://%d", sectors))
	out, err := cmd.Output()
	cancel()
	fields := strings.Fields(string(out))
	if err != nil {
		if len(fields) > 0 {
			detachDisk(fields[0]) // a timeout after the device exists must not leak its RAM
		}
		return "", "", fmt.Errorf("hdiutil attach: %w", err)
	}
	if len(fields) == 0 {
		return "", "", errors.New("hdiutil attach: no device in its output")
	}
	dev = fields[0]
	cmd, cancel = diskCmd("diskutil", "erasevolume", "APFS", volume, dev)
	out, err = cmd.CombinedOutput()
	cancel()
	if err != nil {
		detachDisk(dev)
		return "", "", fmt.Errorf("diskutil erasevolume: %w: %s", err, strings.TrimSpace(string(out)))
	}
	tmp = filepath.Join("/Volumes", volume, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		detachDisk(dev)
		return "", "", err
	}
	return dev, tmp, nil
}

// detachDisk forces the eject: a helper a test leaked must not keep the volume, or its RAM, alive.
func detachDisk(dev string) {
	cmd, cancel := diskCmd("hdiutil", "detach", dev, "-force")
	defer cancel()
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: test ramdisk: detach %s: %v: %s\n", dev, err, strings.TrimSpace(string(out)))
	}
}
