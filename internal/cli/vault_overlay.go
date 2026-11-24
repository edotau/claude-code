package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault/vaultdoc"
)

var vaultOverlayPage = helptext.Page{
	Usage: "claude-code vault overlay <ls|unset> [KEY] [flags]",
	About: "List the vault's overlay keys, or remove one that overrides a built-in default.\n" +
		"Removable keys: " + strings.Join(vaultdoc.OverlayKeys, ", ") + ".",
	Commands: [][2]string{
		{"ls", "every overlay key with its stored size"},
		{"unset", "KEY: remove that overlay document"},
	},
}

// cmdVaultOverlay is `vault overlay ls` and `vault overlay unset KEY`.
func cmdVaultOverlay(args []string) int {
	if code, ok := helpOrBare(vaultOverlayPage, args); ok {
		return code
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "ls":
		return vaultOverlayList(args)
	case "unset":
		return vaultOverlayUnset(args)
	}
	return unknownSub("vault overlay", sub, vaultOverlayPage)
}

func vaultOverlayList(args []string) int {
	fs := helptext.NewFlagSet("vault overlay ls", helptext.Page{Usage: "claude-code vault overlay ls [flags]"})
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	sizes, err := vaultdoc.OverlayStored()
	if err != nil {
		return helptext.Fail("vault overlay ls: %v", err)
	}
	keys := make([]string, 0, len(sizes))
	for k := range sizes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Printf("%s  %d bytes\n", k, sizes[k])
	}
	return 0
}

func vaultOverlayUnset(args []string) int {
	fs := helptext.NewFlagSet("vault overlay unset", helptext.Page{Usage: "claude-code vault overlay unset KEY [flags]"})
	dryRun := fs.Bool("dry-run", false, "report what would be removed; write nothing")
	yes := fs.Bool("yes", false, "remove without asking; required off a terminal")
	fs.BoolVar(yes, "y", false, "same as --yes")
	words, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(words) != 1 {
		fs.Usage()
		return 2
	}
	key := words[0]
	doc, err := vaultdoc.PeekOverlay(key)
	if err != nil {
		return helptext.Fail("vault overlay unset: %v", err)
	}
	if doc == "" {
		fmt.Fprintf(os.Stderr, "overlay %s is not set; nothing written\n", key)
		return 0
	}
	if *dryRun {
		fmt.Fprintf(os.Stderr, "dry run: overlay %s (%d bytes) would be removed; nothing written\n", key, len(doc))
		return 0
	}
	if code, stop := confirmOverlayUnset(key, len(doc), *yes); stop {
		return code
	}
	if err := vaultdoc.UnsetOverlay(key, doc); err != nil {
		return helptext.Fail("vault overlay unset: %v", err)
	}
	fmt.Fprintf(os.Stderr, "overlay %s removed (was %d bytes)\n", key, len(doc))
	return 0
}

// confirmOverlayUnset asks before a removal; stop ends the verb with code.
func confirmOverlayUnset(key string, size int, yes bool) (code int, stop bool) {
	switch {
	case yes:
		return 0, false
	case !tui.StdinIsTerminal():
		fmt.Fprintf(os.Stderr, "vault overlay unset: %s holds %d bytes; pass --yes to remove it\n", key, size)
		return 2, true
	case !tui.ConfirmKey(fmt.Sprintf("remove overlay %s (%d bytes)?", key, size), false):
		fmt.Fprintln(os.Stderr, "vault overlay: nothing changed")
		return 0, true
	}
	return 0, false
}
