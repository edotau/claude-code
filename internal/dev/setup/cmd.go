// Package setup implements installation and the clean sweep.
package setup

import (
	"context"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
)

// CleanSpec is the completion node for the clean verb.
func CleanSpec() *helptext.Node {
	flag, boolFlag := helptext.Plain, helptext.BoolFlag
	return &helptext.Node{Flags: []helptext.Flag{flag("target"), flag("older-than"), boolFlag("dry-run"), boolFlag("yes"), boolFlag("list")}}
}

// InstallSpec is the completion node for the install verb.
func InstallSpec() *helptext.Node {
	return &helptext.Node{Flags: []helptext.Flag{helptext.BoolFlag("dry-run"), helptext.BoolFlag("verbose"), helptext.BoolFlag("missing"), helptext.ValuedFlag("tools", tools.Names)}}
}

// toolsFlag is --tools, bare (every tool) or --tools=name,name2 (that subset only).
type toolsFlag struct {
	set   bool
	names []string
}

func (t *toolsFlag) String() string {
	if t == nil || !t.set {
		return ""
	}
	return strings.Join(t.names, ",")
}

func (t *toolsFlag) Set(s string) error {
	switch s {
	case "false":
		t.set, t.names = false, nil
	case "", "true":
		t.set = true
	default:
		t.set, t.names = true, strings.Split(s, ",")
	}
	return nil
}

func (t *toolsFlag) IsBoolFlag() bool { return true } // allows the bare "--tools" form

func installTools(names []string, dry, missing bool) int {
	if err := tools.Install(context.Background(), os.Stdout, names, dry, missing); err != nil {
		return helptext.Fail("install --tools: %v", err)
	}
	return 0
}

// RunInstall handles install flags.
func RunInstall(args []string) int {
	fs := helptext.NewFlagSet("install", helptext.Page{
		Usage: "claude-code install [flags]",
		About: "Build-time setup: copy claude-code into bin/, link the agent CLIs there, then seed settings.json " +
			"when it is missing. --tools installs or updates only the tools.",
	})
	dry := fs.Bool("dry-run", false, "print what would change; write nothing")
	verbose := fs.Bool("verbose", false, "print every step in full, instead of one line each")
	var tf toolsFlag
	fs.Var(&tf, "tools", "install or update tools only: bare = all, =name,... selects")
	missing := fs.Bool("missing", false, "with --tools: install only absent tools, never update")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.FailUsage("install: unexpected argument %q (flag parsing stops at it); a tool list is --tools=%s", fs.Arg(0), fs.Arg(0))
	}
	if msg := installFlagError(*missing, tf.set); msg != "" {
		return helptext.Fail("%s", msg)
	}
	if tf.set {
		return installTools(tf.names, *dry, *missing)
	}
	if err := install(os.Stdout, *dry, *verbose); err != nil {
		return helptext.Fail("install: %v", err)
	}
	return 0
}

// installFlagError names a misused flag combination, or "" when the flags are consistent.
func installFlagError(missing, toolsSet bool) string {
	if missing && !toolsSet {
		return "install: --missing needs --tools"
	}
	return ""
}
