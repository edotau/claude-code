// Package render is the shared write path for external-agent config files: dry-run/check/write, backups,
// baselines, lost-line detection and revert.
package render

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/edotau/claude-code/internal/paths"
)

// RenderOptions controls one generate pass.
type RenderOptions struct {
	Agents     []string  // agents to render; default DefaultAgents
	DryRun     bool      // print the rendered blocks; write nothing
	Check      bool      // report drift; never writes
	Home       string    // overrides os.UserHomeDir() for agent config paths (tests)
	Out        io.Writer // status lines; nil -> os.Stdout
	ForceHomes bool      // force agent-config renders even when CLAUDE_CONFIG_DIR is not the default ~/.claude
	// Confirm asks whether to overwrite path given its lost hand-edited lines; nil means write and warn.
	Confirm func(path string, lost []string) bool
	// staged is the write queue CommitFile appends to; Stage sets it, RenderPlan.Apply runs it.
	staged *[]stagedWrite
}

// RenderPlan holds ordered agent writes after all selected renders have validated.
type RenderPlan struct {
	writes []stagedWrite
}

// stagedWrite is one queued write; path names the agent config file it changes ("" for a side file like a baseline).
type stagedWrite struct {
	path  string
	write func() error
}

// Apply performs the staged writes in order; on the first error it restores every config file already written.
func (p *RenderPlan) Apply() error {
	var done []snapshot
	for _, w := range p.writes {
		snap, err := takeSnapshot(w.path)
		if err != nil {
			return errors.Join(err, restore(done))
		}
		if err := w.write(); err != nil {
			return errors.Join(err, restore(append(done, snap)))
		}
		done = append(done, snap)
	}
	return nil
}

// snapshot is a config file's bytes before a staged write; absent means it did not exist.
type snapshot struct {
	path   string
	data   []byte
	absent bool
}

func takeSnapshot(path string) (snapshot, error) {
	if path == "" {
		return snapshot{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot{path: path, absent: true}, nil
	}
	return snapshot{path: path, data: data}, err
}

// restore puts snapshots back newest first, so a failed multi-file render leaves no half-applied pair.
func restore(done []snapshot) error {
	var errs []error
	for _, s := range slices.Backward(done) {
		switch {
		case s.path == "":
		case s.absent:
			if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		default:
			errs = append(errs, paths.AtomicWrite(s.path, s.data, 0o600))
		}
	}
	return errors.Join(errs...)
}

// Stage returns o with CommitFile writes queued onto p instead of written.
func (o RenderOptions) Stage(p *RenderPlan) RenderOptions {
	o.staged = &p.writes
	return o
}

// CommitFile is the shared dry-run/check/write decision once a renderer has the next byte slice.
func CommitFile(path string, raw, next []byte, existed bool, o RenderOptions, out io.Writer, label string) (bool, error) {
	changed := !bytes.Equal(next, raw)
	switch {
	case o.DryRun:
		fmt.Fprintf(out, "--- %s (%s) ---\n%s\n", path, label, next)
		return changed, nil
	case o.Check:
		return changed, reportCheck(path, raw, next, changed, out, label)
	case !changed:
		return false, reportUnchanged(path, next, existed, o, out, label)
	}
	*o.staged = append(*o.staged, stagedWrite{path, func() error { return commitWrite(path, raw, next, existed, o, out, label) }})
	return true, nil
}

// reportCheck prints --check drift and any hand-edited lines the render would lose.
func reportCheck(path string, raw, next []byte, changed bool, out io.Writer, label string) error {
	if changed {
		fmt.Fprintf(out, "%s: drift at %s\n", label, path)
	}
	lost, err := lostLinesFor(label, path, raw, next)
	if err != nil {
		return err
	}
	if len(lost) > 0 {
		fmt.Fprintf(out, "%s: %s has %d hand-edited line(s) not in this render:\n", label, path, len(lost))
		printLostLines(out, lost)
	}
	return nil
}

// reportUnchanged prints the "unchanged" line, first seeding or staging the baseline for an unchanged file.
func reportUnchanged(path string, next []byte, existed bool, o RenderOptions, out io.Writer, label string) error {
	if ops := fileOps[label]; ops.LoadBaseline != nil {
		base, err := ops.LoadBaseline(path)
		if err != nil {
			return err
		}
		if base == nil && existed {
			*o.staged = append(*o.staged, stagedWrite{write: func() error { return ops.SaveBaseline(path, next) }})
		}
		fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
		return nil
	}
	// Seed a missing baseline from an unchanged file, or later hand edits go undetected.
	if _, err := os.Stat(baselinePath(label, path)); os.IsNotExist(err) && existed {
		if err := writeBaseline(label, path, next); err != nil {
			fmt.Fprintf(out, "%s: baseline: %v (next run can't detect hand edits)\n", label, err)
		}
	}
	fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
	return nil
}

// commitWrite runs the lost-lines check (prompting or warning), writes path, and refreshes its
// baseline; it is the body of every CommitFile staged write.
func commitWrite(path string, raw, next []byte, existed bool, o RenderOptions, out io.Writer, label string) error {
	lost, err := lostLinesFor(label, path, raw, next)
	if err != nil {
		return err
	}
	if len(lost) > 0 && !confirmLost(path, lost, o, out, label) {
		return nil
	}
	return paths.WithFileLock(baselinePath(label, path), func() error {
		return writeLocked(path, raw, next, existed, out, label)
	})
}

// confirmLost reports lines the write would lose and returns false when the user kept their edits.
func confirmLost(path string, lost []string, o RenderOptions, out io.Writer, label string) bool {
	if o.Confirm == nil {
		fmt.Fprintf(out, "%s: overwrote %d hand-edited line(s) in %s; `claude-code configure --revert` restores\n",
			label, len(lost), path)
		printLostLines(out, lost)
		return true
	}
	fmt.Fprintf(out, "%s: %s has %d hand-edited line(s) that would be lost:\n", label, path, len(lost))
	printLostLines(out, lost)
	if !o.Confirm(path, lost) {
		fmt.Fprintf(out, "%s: kept %s (your edits)\n", label, path)
		return false
	}
	return true
}

// writeLocked re-checks path is unchanged since read, writes it, and refreshes its baseline.
func writeLocked(path string, raw, next []byte, existed bool, out io.Writer, label string) error {
	if cur, err := os.ReadFile(path); (err == nil) != existed || (existed && !bytes.Equal(cur, raw)) {
		return fmt.Errorf("%s: %s changed since it was read; re-run", label, path)
	}
	write := backupAndWrite
	if ops := fileOps[label]; ops.Write != nil {
		write = ops.Write
	}
	if err := write(path, raw, next, existed); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := writeBaseline(label, path, next); err != nil {
		fmt.Fprintf(out, "%s: baseline: %v (next run can't detect hand edits)\n", label, err)
	}
	fmt.Fprintf(out, "%s: wrote %s\n", label, path)
	return nil
}

// CommitOverlayFile is CommitFile's dry-run/check/write decision, minus the lost-hand-edited-lines
// check and baseline tracking: the overlay is never hand-edited, so there is nothing to lose.
func CommitOverlayFile(path string, raw, next []byte, o RenderOptions, out io.Writer, label string) (bool, error) {
	changed := !bytes.Equal(raw, next)
	switch {
	case o.DryRun:
		fmt.Fprintf(out, "--- %s (%s) ---\n%s\n", path, label, next)
		return changed, nil
	case o.Check:
		if changed {
			fmt.Fprintf(out, "%s: drift at %s\n", label, path)
		}
		return changed, nil
	case !changed:
		fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
		return false, nil
	}
	*o.staged = append(*o.staged, stagedWrite{path, func() error {
		if _, err := paths.WriteIfChanged(path, next, 0o600); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		fmt.Fprintf(out, "%s: wrote %s\n", label, path)
		return nil
	}})
	return true, nil
}
