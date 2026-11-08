package themecmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/network/vscode"
	"github.com/edotau/claude-code/internal/statusline"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/theme"
)

var themeStatuslinePage = helptext.Page{
	Usage: "claude-code theme statusline [flags] < payload.json",
	About: "Renders the status line from Claude Code's JSON on stdin (settings.json runs it).",
}

var themePage = helptext.Page{
	Usage: "claude-code theme [<palette>|<command>] [flags]",
	About: "Pick the status line's gradient palette and theme the tools around it. A palette name sets it; " +
		"--palettes lists them.",
	Commands: [][2]string{
		{"apply", "merge theme/<tool>.json into each tool's config"},
		{"statusline", "render the status line (settings.json runs it)"},
		{"vscode", "merge the VS Code Machine settings + keybindings templates"},
	},
	Hint: "claude-code theme <command> -h lists that command's flags.",
}

// Run sets the statusline palette by name, lists them with --palettes, or runs a subcommand; unknown name exits 2.
func Run(args []string) int {
	if code, ok := themeSubcommand(args); ok {
		return code
	}
	fs := helptext.NewFlagSet("theme", themePage)
	palettes := fs.Bool("palettes", false, "list every palette (● current) and change nothing")
	reset := fs.Bool("reset", false, "delete state/statusline/config.json; the default design renders")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *reset {
		return resetThemeConfig()
	}
	if len(fs.Args()) == 0 && !*palettes {
		fs.Usage()
		return 0
	}
	if *palettes {
		printPalettes(os.Stdout, ansi.Enabled(os.Stdout))
		return 0
	}
	if fs.Args()[0] == "apply" {
		return themeApply(fs.Args()[1:])
	}
	return setThemePalette(fs.Args()[0])
}

// themeSubcommand runs the statusline or vscode subcommand; ok is false when args name neither.
func themeSubcommand(args []string) (code int, ok bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "statusline":
		fs := helptext.NewFlagSet("theme statusline", themeStatuslinePage)
		return statusline.Run(fs, args[1:], os.Stdin, os.Stdout), true
	case "vscode":
		return vscode.Run(args[1:]), true
	}
	return 0, false
}

func setThemePalette(name string) int {
	names := theme.GradientNames()
	if !slices.Contains(names, name) {
		fmt.Fprintf(os.Stderr, "claude-code: unknown palette %q — valid palettes: %s\n", name, strings.Join(names, ", "))
		return 2
	}
	if err := theme.SetGradient(name); err != nil {
		return helptext.Fail("%v", err)
	}
	fmt.Printf("statusline palette set to %s\n", name)
	return 0
}

func resetThemeConfig() int {
	removed, err := theme.ResetConfig()
	if err != nil {
		return helptext.Fail("theme --reset: %v", err)
	}
	if !removed {
		fmt.Println("statusline already on the default design (no config.json)")
		return 0
	}
	fmt.Println("statusline override removed; the default design renders")
	return 0
}

// printPalettes writes one line per gradient palette name, swept across its own stops when colored.
func printPalettes(w io.Writer, colored bool) {
	fmt.Fprintln(w, `PALETTES (state/statusline/config.json "gradient"; ● current)`)
	mode, _, _ := theme.Theme()
	current := theme.CurrentGradient()
	for _, name := range theme.GradientNames() {
		mark := " "
		if name == current {
			mark = "●"
		}
		if colored {
			fmt.Fprintf(w, "  %s %s\n", mark, ansi.Sweep(mode, theme.GradientStops(name), name))
			continue
		}
		fmt.Fprintf(w, "  %s %s\n", mark, name)
	}
}

// themeApply is `claude-code theme apply [--only a,b]`: merge every theme/<tool>.json|.conf into that tool's config.
func themeApply(args []string) int {
	fs := helptext.NewFlagSet("theme apply", helptext.Page{
		Usage: "claude-code theme apply [flags]",
		About: "Merge every theme/<tool>.json|.conf into that tool's config.",
	})
	dryRun := fs.Bool("dry-run", false, "report what would change; write nothing")
	only := fs.String("only", "", "apply only these comma-separated targets (e.g. opencode-themes)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return helptext.Fail("theme apply: %v", err)
	}
	var names []string
	if *only != "" {
		names = strings.Split(*only, ",")
	}
	out, err := theme.Apply(theme.Dir(), home, names, *dryRun)
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
		return helptext.Fail("theme apply: %v", err)
	}
	return 0
}

// Spec is the completion node for the theme verb tree.
func Spec() *completion.Node {
	boolFlag := func(n string) completion.Flag { return completion.Flag{Name: n, IsBool: true} }
	styles := func() []string { return []string{"gradient", "powerline", "capsule", "minimal", "dashboard"} }
	return &completion.Node{Args: []func() []string{theme.GradientNames}, Flags: []completion.Flag{boolFlag("palettes"), boolFlag("reset")},
		Sub: map[string]*completion.Node{
			"apply":      {Flags: []completion.Flag{boolFlag("dry-run")}},
			"statusline": {Flags: []completion.Flag{{Name: "style", Values: styles}}},
			"vscode":     vscode.Spec(),
		}}
}
