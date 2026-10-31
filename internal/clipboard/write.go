package clipboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// DefaultClearAfter is how long a copied secret stays on the clipboard before the timed clear.
const DefaultClearAfter = 30 * time.Second

// ErrNoClipboard means no supported clipboard tool was found for this platform.
var ErrNoClipboard = errors.New("no clipboard tool available (need powershell.exe, pbcopy, wl-copy or xclip)")

// run is the exec seam: it runs name with stdin and returns stdout; tests replace it to assert argv.
var run = func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		// Command name and status only: never stderr or stdin, which may echo the value.
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("%s: exit status %d", name, ee.ExitCode())
		}
		return nil, fmt.Errorf("%s: could not run", name)
	}
	return out.Bytes(), nil
}

// spawnClear is the detach seam for ScheduleClear.
var spawnClear = func(self string, args []string, stdin []byte) error {
	// A real pipe filled before Start: a bytes.Reader is copied by a goroutine that dies when we exit.
	r, w, err := os.Pipe()
	if err != nil {
		return errors.New("clip-clear: could not start")
	}
	defer r.Close()
	_, werr := w.Write(stdin)
	_ = w.Close()
	cmd := exec.Command(self, args...)
	cmd.Stdin = r
	proc.Detach(cmd)
	if werr != nil || cmd.Start() != nil {
		return errors.New("clip-clear: could not start")
	}
	return cmd.Process.Release()
}

// selfPath resolves the running binary; a var so tests can pin it.
var selfPath = os.Executable

// onWSL reports whether we are under WSL; a var so tests can force either branch.
var onWSL = proc.OnWSL

type tool struct {
	name        string
	write, read []string // argv after name
	clear       []string // nil = write empty stdin
}

// wslPowerShell is where WSL has Windows PowerShell when interop leaves it off PATH.
var wslPowerShell = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"

// SetWSLForTest pins WSL detection for other packages' tests and returns the restore.
func SetWSLForTest(on bool) func() {
	old := onWSL
	onWSL = func() bool { return on }
	return func() { onWSL = old }
}

const psPre = "[Console]::InputEncoding=[Text.Encoding]::UTF8;[Console]::OutputEncoding=[Text.Encoding]::UTF8;" +
	"Add-Type -AssemblyName System.Windows.Forms;"

var psArgs = []string{"-NoProfile", "-NonInteractive", "-Sta"}

func psCmd(script string) []string { return append(append([]string{}, psArgs...), "-Command", script) }

// psWrite reads stdin, marks the data off Win+V history/cloud sync, then sets the clipboard.
const psWrite = psPre + "$t=[Console]::In.ReadToEnd();$d=New-Object System.Windows.Forms.DataObject;" +
	"$d.SetText($t);$d.SetData('ExcludeClipboardContentFromMonitorProcessing',$true);" +
	"[System.Windows.Forms.Clipboard]::SetDataObject($d,$true)"

const psRead = psPre + "$c=Get-Clipboard -Raw;if($c -ne $null){[Console]::Out.Write($c)}"

const psClear = psPre + "[System.Windows.Forms.Clipboard]::Clear()"

func pick() (tool, error) {
	has := func(n string) bool { _, err := exec.LookPath(n); return err == nil }
	ps := "powershell.exe"
	if !has(ps) {
		if _, err := os.Stat(wslPowerShell); err == nil {
			ps = wslPowerShell
		}
	}
	switch {
	case onWSL() && (has(ps) || ps == wslPowerShell):
		return tool{ps, psCmd(psWrite), psCmd(psRead), psCmd(psClear)}, nil
	case has("pbcopy"):
		return tool{"pbcopy", nil, nil, nil}, nil
	case os.Getenv("WAYLAND_DISPLAY") != "" && has("wl-copy"):
		return tool{"wl-copy", nil, []string{"-n"}, []string{"--clear"}}, nil
	case os.Getenv("DISPLAY") != "" && has("xclip"):
		return tool{"xclip", []string{"-selection", "clipboard", "-i"}, []string{"-selection", "clipboard", "-o"}, nil}, nil
	}
	return tool{}, ErrNoClipboard
}

// WriteSecret puts value on the clipboard; the value travels on stdin only, never argv.
func WriteSecret(ctx context.Context, value string) error {
	t, err := pick()
	if err != nil {
		return err
	}
	_, err = run(ctx, []byte(value), t.name, t.write...)
	return err
}

// Digest is the sha256 hex digest ScheduleClear/ClearIfMatch compare against.
func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// ScheduleClear spawns a detached `clip-clear --after <after>` child that clears the clipboard if it
// still holds value; only the sha256 digest crosses to the child, on stdin.
func ScheduleClear(after time.Duration, value string) error {
	if testing.Testing() {
		return errors.New("clip-clear: refused under test")
	}
	self, err := selfPath()
	if err != nil {
		return errors.New("clip-clear: cannot locate own binary")
	}
	return spawnClear(self, []string{"clip-clear", "--after", after.String()}, []byte(Digest(value)))
}

// ClearIfMatch clears the clipboard only when its text hashes to digest; it reports whether it cleared.
func ClearIfMatch(ctx context.Context, digest string) (bool, error) {
	t, err := pick()
	if err != nil {
		return false, err
	}
	cur, err := readText(ctx, t)
	if err != nil {
		return false, err
	}
	digest = strings.ToLower(strings.TrimSpace(digest))
	if !digestMatches(cur, digest) {
		return false, nil
	}
	if t.clear != nil {
		_, err = run(ctx, nil, t.name, t.clear...)
	} else {
		_, err = run(ctx, []byte{}, t.name, t.write...)
	}
	return err == nil, err
}

func readText(ctx context.Context, t tool) ([]byte, error) {
	if t.name == "wl-copy" {
		return run(ctx, nil, "wl-paste", t.read...) // wl-copy writes; its paste side is a separate binary
	}
	if t.read != nil {
		return run(ctx, nil, t.name, t.read...)
	}
	return run(ctx, nil, "pbpaste")
}

// digestMatches tries the text as read and without a trailing newline some tools append.
func digestMatches(cur []byte, digest string) bool {
	for _, c := range []string{string(cur), strings.TrimRight(string(cur), "\r\n")} {
		if c != "" && subtle.ConstantTimeCompare([]byte(Digest(c)), []byte(digest)) == 1 {
			return true
		}
	}
	return false
}
