package cli

import (
	"encoding/json"
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

var themeStatuslinePage = helptext.Page{
	Usage: "claude-code theme statusline [flags] < payload.json",
	About: "Renders the status line from Claude Code's JSON on stdin (settings.json runs it).",
}

var themePage = helptext.Page{
	Usage: "claude-code theme [<palette>|<command>] [flags]",
	About: "Pick the status line's gradient palette and theme the tools around it. Bare shows this help " +
		"and every palette; a palette name sets it.",
	Commands: [][2]string{
		{"apply", "merge theme/<tool>.json into each tool's config"},
		{"statusline", "render the status line (settings.json runs it)"},
		{"vscode", "merge the VS Code Machine settings + keybindings templates"},
	},
	Hint: "claude-code theme <command> -h lists that command's flags.",
}

// cmdTheme picks the statusline's gradient palette persistently. No argument (or --preview) lists
// every palette, painted in its own gradient on a terminal; a name sets it; an unknown name exits 2.
func cmdTheme(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "statusline":
			fs := helptext.NewFlagSet("theme statusline", themeStatuslinePage)
			return statusline.Run(fs, args[1:], os.Stdin, os.Stdout)
		case "vscode":
			return cmdVSCode(args[1:])
		}
	}
	fs := helptext.NewFlagSet("theme", themePage)
	fs.Usage = func() { // help and the palettes it chooses from, so bare `theme` and `-h` show both
		p := themePage
		p.Flags = fs
		p.Fprint(os.Stderr)
		fmt.Fprintln(os.Stderr)
		printPalettes(os.Stderr, colorOK(os.Stderr))
	}
	preview := fs.Bool("preview", false, "print every palette and change nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if len(fs.Args()) == 0 && !*preview {
		fs.Usage()
		return 0
	}
	if *preview {
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
	fmt.Fprintln(w, `PALETTES (state/statusline/config.json "gradient"; ● current)`)
	mode, _, _ := statusline.Theme()
	current := currentGradient()
	for _, name := range statusline.GradientNames() {
		mark := " "
		if name == current {
			mark = "●"
		}
		if colored {
			fmt.Fprintf(w, "  %s %s\n", mark, sweep(mode, statusline.GradientStops(name), name))
			continue
		}
		fmt.Fprintf(w, "  %s %s\n", mark, name)
	}
}

// currentGradient is the palette the status line uses now: the user config's "gradient", else the default's.
func currentGradient() string {
	var cfg struct {
		Gradient string `json:"gradient"`
	}
	if raw, err := os.ReadFile(filepath.Join(statusline.ConfigDir(), "config.json")); err == nil {
		if json.Unmarshal(raw, &cfg) == nil && cfg.Gradient != "" {
			return cfg.Gradient
		}
	}
	_ = json.Unmarshal(statusline.DefaultConfigJSON(), &cfg)
	return cfg.Gradient
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
	fs := helptext.NewFlagSet("theme apply", helptext.Page{
		Usage: "claude-code theme apply [flags]",
		About: "Merge every theme/<tool>.json into that tool's config.",
	})
	dryRun := fs.Bool("dry-run", false, "report what would change; write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
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
