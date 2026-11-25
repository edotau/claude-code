// Package launchd installs per-user macOS launch agents: write the plist when it changed, then bootstrap it.
package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// Agent is one launch agent; each domain and label must use one canonical plist path across installs.
type Agent struct {
	Label, Plist, Domain string       // Domain is "gui/<uid>"
	Body                 []byte       // rendered plist
	Prep                 func() error // creates directories needed by the job
}

const ctlTimeout = 30 * time.Second // bounds each launchctl call so a wedged launchd cannot hang install

// Ctl runs launchctl with a timeout and folds its output into the error. Tests replace it.
var Ctl = ctl

// Install writes and loads a, leaving an unchanged loaded plist alone; not macOS, it does nothing.
func Install(w io.Writer, a Agent, dryRun bool) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return installSerialized(w, a, dryRun)
}

// installSerialized is Install without the darwin gate; dry runs never create lock files.
func installSerialized(w io.Writer, a Agent, dryRun bool) error {
	if dryRun {
		return install(w, a, true)
	}
	// The canonical plist lock serializes decisions and reloads across processes.
	return paths.WithFileLock(a.Plist, func() error { return install(w, a, false) })
}

func install(w io.Writer, a Agent, dryRun bool) error {
	switch same, err := plistCurrent(a); {
	case err != nil:
		return err
	case same:
		return current(w, a, dryRun)
	}
	if dryRun {
		fmt.Fprintf(w, "  %-17s %s\n", rowName(a.Label), "would write "+a.Plist+" (skipped: --dry-run)")
		return nil
	}
	if err := prep(a); err != nil {
		return err
	}
	// Mark intent before publishing so any interrupted reload will be retried.
	if err := paths.AtomicWrite(a.Plist+".pending", nil, 0o600); err != nil {
		return fmt.Errorf("mark launchd reload pending: %w", err)
	}
	if err := paths.AtomicWrite(a.Plist, a.Body, 0o644); err != nil {
		return err
	}
	if err := reload(a); err != nil {
		return err
	}
	fmt.Fprintf(w, "  %-17s %s\n", rowName(a.Label), "wrote "+a.Plist)
	return nil
}

// reload retains the pending marker until bootstrap succeeds.
func reload(a Agent) error {
	if err := Ctl("bootout", a.Domain+"/"+a.Label); err != nil && !notBootedOut(err) {
		return fmt.Errorf("launchctl bootout: %w", err)
	}
	if err := Ctl("bootstrap", a.Domain, a.Plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	if err := os.Remove(a.Plist + ".pending"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear launchd reload pending: %w", err)
	}
	return nil
}

// plistCurrent reports whether the plist matches and no reload was interrupted.
func plistCurrent(a Agent) (bool, error) {
	if _, err := os.Lstat(a.Plist + ".pending"); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	have, err := os.ReadFile(a.Plist)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bytes.Equal(have, a.Body), nil
}

// current handles a plist that already matches Body: leave a loaded one alone, else load it.
func current(w io.Writer, a Agent, dryRun bool) error {
	if err := Ctl("print", a.Domain+"/"+a.Label); err == nil {
		fmt.Fprintf(w, "  %-17s %s\n", rowName(a.Label), "current: "+a.Plist)
		return nil
	} else if !notLoaded(err) {
		return err
	}
	if dryRun {
		fmt.Fprintf(w, "  %-17s %s\n", rowName(a.Label), "would load "+a.Plist+" (skipped: --dry-run)")
		return nil
	}
	if err := prep(a); err != nil {
		return err
	}
	if err := Ctl("bootstrap", a.Domain, a.Plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	fmt.Fprintf(w, "  %-17s %s\n", rowName(a.Label), "loaded "+a.Plist)
	return nil
}

// notLoaded reports launchctl print saying the domain has no such service (exit 113).
func notLoaded(err error) bool {
	return strings.Contains(err.Error(), "Could not find service")
}

// notBootedOut recognizes launchctl's explicit absent-service responses to bootout.
func notBootedOut(err error) bool {
	var status interface{ ExitCode() int }
	if !errors.As(err, &status) {
		return false
	}
	switch status.ExitCode() {
	case 113:
		return notLoaded(err)
	case 3:
		return strings.Contains("\n"+err.Error()+"\n", ": Boot-out failed: 3: No such process\n")
	}
	return false
}

// prep runs the agent's directory setup; an agent with nothing to prepare is fine.
func prep(a Agent) error {
	if a.Prep == nil {
		return nil
	}
	return a.Prep()
}

// rowName is the install-table row: the label's last dot segment (clip, dream) plus " agent".
func rowName(label string) string {
	if i := strings.LastIndex(label, "."); i >= 0 {
		label = label[i+1:]
	}
	return label + " agent"
}

// ctl runs launchctl with a timeout and folds its output into the error.
func ctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), ctlTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if err != nil {
		return joinCtl(err, out)
	}
	return nil
}

// joinCtl appends launchctl's output to err; outputless failures keep err as it is.
func joinCtl(err error, out []byte) error {
	if msg := bytes.TrimSpace(out); len(msg) > 0 {
		return fmt.Errorf("%w: %s", err, msg)
	}
	return err
}

// XMLText escapes s for a plist <string> body, so a path with & or < stays well-formed.
func XMLText(s string) (string, error) {
	var b bytes.Buffer
	err := xml.EscapeText(&b, []byte(s))
	return b.String(), err
}
