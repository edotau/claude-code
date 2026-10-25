package databricks

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/databricks/dbxcfg"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
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
	if rest, err := outfmt.Parse(fs, args); err != nil {
		return helptext.ExitCode(err)
	} else if len(rest) > 0 {
		fs.Usage()
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("export: %v", err)
	}
	rows, skipped := dbxcfg.ExportRows(reg)
	if *pick && len(rows) > 0 {
		if rows, err = pickExport(rows); err != nil {
			return fail("export: %v", err)
		}
	}
	rows, more := dbxcfg.Rename(rows, rename)
	for _, s := range append(skipped, more...) {
		fmt.Fprintln(os.Stderr, "skip "+s)
	}
	if len(rows) == 0 {
		return fail("export: no databricks rows with an authType to export")
	}
	if code := writeExport(rows, *out, *merge, *force); code != 0 {
		return code
	}
	for _, r := range rows {
		fmt.Fprintln(os.Stderr, "use: "+dbxcfg.Hint(r))
	}
	return 0
}

func writeExport(rows []dbxcfg.ExportRow, out string, merge, force bool) int {
	if !merge {
		text := dbxcfg.Render(rows)
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
		out = dbxcfg.DefaultPath()
	}
	old, err := os.ReadFile(out)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail("export: %v", err)
	}
	text, notes := dbxcfg.Merge(string(old), rows, force)
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, n)
	}
	if text == string(old) {
		fmt.Fprintf(os.Stderr, "%s unchanged\n", out)
		return 0
	}
	if err := writeFile0600(out, text, true); err != nil {
		return fail("export: %v", err)
	}
	fmt.Fprintf(os.Stderr, "merged into %s\n", out)
	return 0
}

func pickExport(rows []dbxcfg.ExportRow) ([]dbxcfg.ExportRow, error) {
	labels := make([][2]string, len(rows))
	for i, r := range rows {
		labels[i] = [2]string{r.ID, r.Auth + " " + r.Host}
	}
	on, err := pickToggle("Export rows (Enter toggles)", "export the checked rows", labels)
	if err != nil {
		return nil, err
	}
	var out []dbxcfg.ExportRow
	for i, r := range rows {
		if on[i] {
			out = append(out, r)
		}
	}
	return out, nil
}

// writeFile0600 writes text to path through a 0600 temp file and a rename; an existing path needs force.
func writeFile0600(path, text string, force bool) error {
	if _, err := os.Lstat(path); err == nil && !force {
		return fmt.Errorf("%s exists; pass --force to overwrite", path)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.WriteString(tmp, text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
