// Package render is the shared write path for external-agent config files: dry-run/check/write, backups,
// baselines, lost-line detection and revert.
package render

import (
	"bytes"
	"fmt"
	"io"
	"os"

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
	staged *[]func() error
}

// RenderPlan holds ordered agent writes after all selected renders have validated.
type RenderPlan struct {
	writes []func() error
}

// Apply performs the staged writes in order, stopping at the first error.
func (p *RenderPlan) Apply() error {
	for _, write := range p.writes {
		if err := write(); err != nil {
			return err
		}
	}
	return nil
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
		if changed {
			fmt.Fprintf(out, "%s: drift at %s\n", label, path)
		}
		lost, err := lostLinesFor(label, path, raw, next)
		if err != nil {
			return changed, err
		}
		if len(lost) > 0 {
			fmt.Fprintf(out, "%s: %s has %d hand-edited line(s) not in this render:\n", label, path, len(lost))
			printLostLines(out, lost)
		}
		return changed, nil
	case !changed:
		if ops := fileOps[label]; ops.LoadBaseline != nil {
			base, err := ops.LoadBaseline(path)
			if err != nil {
				return false, err
			}
			if base == nil && existed {
				*o.staged = append(*o.staged, func() error { return ops.SaveBaseline(path, next) })
			}
			fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
			return false, nil
		}
		// Seed a missing baseline from an unchanged file, or later hand edits go undetected.
		if _, err := os.Stat(baselinePath(label, path)); os.IsNotExist(err) && existed {
			_ = writeBaseline(label, path, next)
		}
		fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
		return false, nil
	}
	*o.staged = append(*o.staged, func() error { return commitWrite(path, raw, next, existed, o, out, label) })
	return true, nil
}

// commitWrite runs the lost-lines check (prompting or warning), writes path, and refreshes its
// baseline; it is the body of every CommitFile staged write.
func commitWrite(path string, raw, next []byte, existed bool, o RenderOptions, out io.Writer, label string) error {
	lost, err := lostLinesFor(label, path, raw, next)
	if err != nil {
		return err
	}
	if len(lost) > 0 {
		if o.Confirm != nil {
			fmt.Fprintf(out, "%s: %s has %d hand-edited line(s) that would be lost:\n", label, path, len(lost))
			printLostLines(out, lost)
			if !o.Confirm(path, lost) {
				fmt.Fprintf(out, "%s: kept %s (your edits)\n", label, path)
				return nil
			}
		} else {
			fmt.Fprintf(out, "%s: overwrote %d hand-edited line(s) in %s; `claude-code configure --revert` restores\n",
				label, len(lost), path)
			printLostLines(out, lost)
		}
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
	*o.staged = append(*o.staged, func() error {
		if _, err := paths.WriteIfChanged(path, next, 0o600); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		fmt.Fprintf(out, "%s: wrote %s\n", label, path)
		return nil
	})
	return true, nil
}
