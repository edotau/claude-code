package databricks

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var exportPage = helptext.Page{
	Usage: "claude-code databricks export [flags]",
	About: "Write the vault's databricks rows as .databrickscfg sections. No secret is\n" +
		"ever written: each row's use-time hint goes to stderr. Prints to stdout\n" +
		"unless --out; --merge edits an existing file in place.",
}

// export renders vault rows as .databrickscfg sections: stdout, a new --out file, or --merge into one.
func export(args []string) int {
	fs := helptext.NewFlagSet("databricks export", exportPage)
	pick := fs.Bool("pick", false, "choose rows in a picker (default: all)")
	out := fs.String("out", "", "write to FILE (0600) instead of stdout")
	merge := fs.Bool("merge", false, "merge into --out (default the CLI's config), others kept")
	force := fs.Bool("force", false, "overwrite --out, or with --merge replace same-name sections")
	rename := renameFlag{}
	fs.Var(rename, "rename", "write row OLD as section NEW (repeatable)")
	if rest, err := helptext.ParseArgs(fs, args); err != nil {
		return helptext.ExitCode(err)
	} else if len(rest) > 0 {
		fs.Usage()
		return 2
	}
	rows, code := exportRowsFor(*pick, rename)
	if code != 0 {
		return code
	}
	if code := writeExport(rows, *out, *merge, *force); code != 0 {
		return code
	}
	for _, r := range rows {
		fmt.Fprintln(os.Stderr, "use: "+Hint(r))
	}
	return 0
}

// exportRowsFor loads the vault rows, applies --pick and --rename, reports skips, and returns the rows to write
// (or a nonzero exit code).
func exportRowsFor(pick bool, rename renameFlag) ([]ExportRow, int) {
	reg, err := providers.Load()
	if err != nil {
		return nil, fail("export: %v", err)
	}
	rows, skipped := ExportRows(reg)
	if pick && len(rows) > 0 {
		if rows, err = pickExport(rows); err != nil {
			return nil, fail("export: %v", err)
		}
	}
	rows, more := Rename(rows, rename)
	for _, s := range append(skipped, more...) {
		fmt.Fprintln(os.Stderr, "skip "+s)
	}
	if len(rows) == 0 {
		return nil, fail("export: no databricks rows with an authType to export")
	}
	return rows, 0
}

func writeExport(rows []ExportRow, out string, merge, force bool) int {
	if !merge {
		text := RenderExport(rows)
		if out == "" {
			_, _ = io.WriteString(os.Stdout, text)
			return 0
		}
		if err := writeFile0600(out, text, force); err != nil {
			return fail("export: %v", err)
		}
		fmt.Fprintf(os.Stderr, "wrote %d sections to %s\n", len(rows), out)
		return 0
	}
	if out == "" {
		out = DefaultPath()
	}
	var msg string
	if err := paths.WithFileLock(out, func() error {
		var err error
		msg, err = mergeInto(out, rows, force)
		return err
	}); err != nil {
		return fail("export: %v", err)
	}
	fmt.Fprint(os.Stderr, msg)
	return 0
}

// mergeInto merges rows into the config at out (the caller holds its lock) and returns the stderr summary.
func mergeInto(out string, rows []ExportRow, force bool) (string, error) {
	old, err := os.ReadFile(out)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := parseSections(string(old)); err != nil {
		return "", fmt.Errorf("%s: %w", out, err)
	}
	text, notes := Merge(string(old), rows, force)
	msg := strings.Join(notes, "\n")
	if len(notes) > 0 {
		msg += "\n"
	}
	if text == string(old) {
		return msg + out + " unchanged\n", nil
	}
	if err := paths.AtomicWrite(out, []byte(text), 0o600); err != nil {
		return "", err
	}
	return msg + "merged into " + out + "\n", nil
}

func pickExport(rows []ExportRow) ([]ExportRow, error) {
	labels := make([][2]string, len(rows))
	for i, r := range rows {
		labels[i] = [2]string{r.ID, r.Auth + " " + r.Host}
	}
	on, err := pickToggle("Export rows (Enter toggles)", "export the checked rows", labels)
	if err != nil {
		return nil, err
	}
	var out []ExportRow
	for i, r := range rows {
		if on[i] {
			out = append(out, r)
		}
	}
	return out, nil
}

// writeFile0600 writes text to path atomically with mode 0600; an existing path needs force.
func writeFile0600(path, text string, force bool) error {
	return paths.WithFileLock(path, func() error {
		if _, err := os.Lstat(path); err == nil && !force {
			return fmt.Errorf("%s exists; pass --force to overwrite", path)
		}
		return paths.AtomicWrite(path, []byte(text), 0o600)
	})
}
