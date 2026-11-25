package cli

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxyteam"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaultdoc"
)

var vaultOverlayPage = helptext.Page{
	Usage: "claude-code vault overlay <ls|set|unset> [KEY] [flags]",
	About: "List the vault's overlay keys, validate and store one, or remove one.\n" +
		vaultOverlaySetAbout,
	Commands: [][2]string{
		{"ls", "every overlay key with its stored size"},
		{"set", "KEY --file PATH|-: validate, store, audit"},
		{"unset", "KEY: remove that overlay document"},
	},
}

const vaultOverlaySetAbout = "Settable keys: proxy-teams, providers, agent-records, opencode-models,\n" +
	"github-auth. providers.prev is a backup and cannot be set.\n" +
	"github-auth is a plain-text profile name; other keys are JSON.\n" +
	"Set prints byte sizes only, never document values."

// cmdVaultOverlay is `vault overlay ls`, `set KEY --file PATH|-` and `unset KEY`.
func cmdVaultOverlay(args []string) int {
	if code, ok := helpOrBare(vaultOverlayPage, args); ok {
		return code
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "ls":
		return vaultOverlayList(args)
	case "set":
		return vaultOverlaySet(args)
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

// validateAgentRecords parses the document and runs the configure-path validator on every record.
func validateAgentRecords(doc []byte) error {
	recs, err := vault.ParseAgentRecords(doc)
	if err != nil { // a decode error would echo the document's keys
		return errors.New("not a JSON object of agent records")
	}
	for _, n := range slices.Sorted(maps.Keys(recs)) {
		if err = agentpkg.ValidateRecord(n, recs[n]); err != nil {
			break
		}
	}
	return err
}

// overlayValidators hold each settable key's own parser; a key with none is refused.
var overlayValidators = map[string]func(doc []byte) error{
	"providers":     providers.Validate,
	"agent-records": validateAgentRecords,
	"opencode-models": func(doc []byte) error {
		_, err := providers.ParseOpencodeMenu(doc)
		return err
	},
	"github-auth": func(doc []byte) error {
		_, err := vault.ParseGitHubAuth(doc)
		return err
	},
	"proxy-teams": func(doc []byte) error {
		_, ps := proxyteam.Parse(assets.ProxyTeams(), doc)
		if len(ps) > 0 {
			return errors.New("invalid proxy-teams document")
		}
		return nil
	},
}

func vaultOverlaySet(args []string) int {
	fs := helptext.NewFlagSet("vault overlay set", helptext.Page{
		Usage: "claude-code vault overlay set KEY --file PATH|-",
		About: vaultOverlaySetAbout,
	})
	file := fs.String("file", "", "document to store: a path, or - for stdin")
	words, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(words) != 1 || *file == "" {
		fs.Usage()
		return 2
	}
	key := words[0]
	validate, ok := overlayValidators[key]
	if !ok {
		return helptext.Fail("vault overlay set: key has no validator; settable keys: %s", strings.Join(vaultdoc.SettableOverlayKeys, ", "))
	}
	raw, err := readOverlayDoc(*file)
	if err != nil {
		return helptext.Fail("vault overlay set: %v", err)
	}
	if err := validate(raw); err != nil {
		why := "invalid document"
		if key == vault.AgentRecordsKey { // validateAgentRecords errors never echo a value
			why = err.Error()
		}
		return helptext.Fail("vault overlay set: %s refused, nothing written (%s)", key, why)
	}
	old, err := vaultdoc.SetOverlay(key, string(raw))
	if err != nil {
		return helptext.Fail("vault overlay set: %v", err)
	}
	audit.Log(audit.OverlaySet, key, fmt.Sprintf("%d -> %d bytes", old, len(raw)))
	fmt.Fprintf(os.Stderr, "overlay %s set: %d -> %d bytes\n", key, old, len(raw))
	return 0
}

func readOverlayDoc(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}
