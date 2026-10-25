package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/statusline"
	"github.com/edotau/claude-code/internal/theme"
)

const themeStatuslineUsage = `usage: claude-code theme statusline [--style gradient|powerline|capsule|
                                    minimal|dashboard]

Renders the status line from Claude Code's JSON on stdin (settings.json runs it)

`

const themeUsage = `usage: claude-code theme [<palette>|--preview]
       claude-code theme apply [--dry-run]
       claude-code theme statusline [--style gradient|powerline|capsule|
                                    minimal|dashboard]
       claude-code theme vscode [--check] [--dry-run] [--dest f] [--template f]
                                [--keybindings-dest f]
                                [--keybindings-template f]
                                [--extensions-dir d]

bare lists every palette; a name sets it. apply merges theme/<tool>.json into
each tool. statusline renders the status line (settings.json runs it).
`

// cmdTheme picks the statusline's gradient palette persistently. No argument (or --preview) lists
// every palette, painted in its own gradient on a terminal; a name sets it; an unknown name exits 2.
func cmdTheme(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "statusline":
			fs := helptext.NewFlagSet("theme statusline", themeStatuslineUsage)
			return statusline.Run(fs, args[1:], os.Stdin, os.Stdout)
		case "vscode":
			return cmdVSCode(args[1:])
		}
	}
	fs := flag.NewFlagSet("theme", flag.ContinueOnError)
	fs.Usage = func() { helptext.Fprint(os.Stderr, themeUsage) }
	preview := fs.Bool("preview", false, "print every palette and change nothing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *preview || len(fs.Args()) == 0 {
		printPalettes(os.Stdout, colorOK(os.Stdout))
		return 0
	}
	if fs.Args()[0] == "apply" {
		return themeApply(fs.Args()[1:])
	}
	name := fs.Args()[0]
	names := statusline.GradientNames()
	if !slices.Contains(names, name) {
		fmt.Fprintf(os.Stderr, "claude-code: unknown palette %q — valid palettes: %s\n", name, strings.Join(names, ", "))
		return 2
	}
	if err := setGradient(name); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("statusline palette set to %s\n", name)
	return 0
}

// printPalettes writes one line per gradient palette name, swept across its own stops when colored.
func printPalettes(w io.Writer, colored bool) {
	fmt.Fprintln(w, `Statusline palettes — state/statusline/config.json "gradient"`)
	fmt.Fprintln(w)
	mode, _, _ := statusline.Theme()
	for _, name := range statusline.GradientNames() {
		if colored {
			fmt.Fprintf(w, "  %s\n", sweep(mode, statusline.GradientStops(name), name))
			continue
		}
		fmt.Fprintf(w, "  %s\n", name)
	}
}

// setGradient merges "gradient":name into state/statusline/config.json, creating it from the
// embedded default when absent. It never touches any other key, and never touches settings.json.
func setGradient(name string) error {
	path := filepath.Join(statusline.ConfigDir(), "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		raw = statusline.DefaultConfigJSON()
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if m == nil { // "null" decodes without error but leaves m nil
		return fmt.Errorf("parse %s: not a JSON object", path)
	}
	encoded, err := json.Marshal(name)
	if err != nil {
		return err
	}
	m["gradient"] = encoded
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(path, append(out, '\n'), 0o644)
}

// themeApply is `claude-code theme apply [--dry-run]`: merge every theme/<tool>.json into that tool's config.
func themeApply(args []string) int {
	fs := flag.NewFlagSet("theme apply", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of theme apply:\n")
		helptext.Defaults(fs)
	}
	dryRun := fs.Bool("dry-run", false, "report what would change; write nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail("theme apply: %v", err)
	}
	out, err := theme.Apply(theme.Dir(), home, *dryRun)
	for _, o := range out {
		switch {
		case o.Skipped != "":
			fmt.Printf("  -  %-10s skipped: %s\n", o.Name, o.Skipped)
		case !o.Changed:
			fmt.Printf("  =  %-10s %s (already applied)\n", o.Name, o.Dest)
		case *dryRun:
			fmt.Printf("  ~  %-10s would update %s\n", o.Name, o.Dest)
		case o.Backup == "":
			fmt.Printf("  ✓  %-10s created %s\n", o.Name, o.Dest)
		default:
			fmt.Printf("  ✓  %-10s updated %s (backup %s)\n", o.Name, o.Dest, filepath.Base(o.Backup))
		}
	}
	if err != nil {
		return fail("theme apply: %v", err)
	}
	return 0
}
