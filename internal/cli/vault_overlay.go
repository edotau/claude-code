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
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/cli/ui/tui"
	"github.com/edotau/claude-code/internal/dev/vaultdoc"
	"github.com/edotau/claude-code/internal/platform/audit"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/proxy/proxyteam"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// overlaySetAbout is the settable-key help, listing the keys of overlayTable.
func overlaySetAbout() string {
	keys := make([]string, len(overlayTable))
	for i, e := range overlayTable {
		keys[i] = e.key
	}
	return "Settable keys: " + strings.Join(keys, ", ") + ".\n" +
		"providers.prev is a backup and cannot be set. All settable keys are JSON.\n" +
		"Set prints byte sizes only, never document values."
}

// vaultOverlayList is `vault ls overlays`: every overlay key with its stored size.
func vaultOverlayList() int {
	sizes, err := vaultdoc.OverlayStored()
	if err != nil {
		return helptext.Fail("vault ls overlays: %v", err)
	}
	for _, k := range slices.Sorted(maps.Keys(sizes)) {
		fmt.Printf("%s  %d bytes\n", k, sizes[k])
	}
	return 0
}

// vaultOverlayUnset is `vault rm overlay KEY`.
func vaultOverlayUnset(key string, dryRun, yes bool) int {
	doc, err := vaultdoc.PeekOverlay(key)
	if err != nil {
		return helptext.Fail("vault rm overlay: %v", err)
	}
	if doc == "" {
		fmt.Fprintf(os.Stderr, "overlay %s is not set; nothing written\n", key)
		return 0
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "dry run: overlay %s (%d bytes) would be removed; nothing written\n", key, len(doc))
		return 0
	}
	if code, stop := confirmOverlayUnset(key, len(doc), yes); stop {
		return code
	}
	if err := vaultdoc.UnsetOverlay(key, doc); err != nil {
		return helptext.Fail("vault rm overlay: %v", err)
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
		fmt.Fprintf(os.Stderr, "vault rm overlay: %s holds %d bytes; pass --yes to remove it\n", key, size)
		return 2, true
	case !tui.ConfirmKey(fmt.Sprintf("remove overlay %s (%d bytes)?", key, size), false):
		fmt.Fprintln(os.Stderr, "vault: nothing changed")
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
	if err := vault.CheckRecordKeys(doc); err != nil {
		return err
	}
	for _, n := range slices.Sorted(maps.Keys(recs)) {
		if err = agentpkg.ValidateRecord(n, recs[n]); err != nil {
			break
		}
	}
	return err
}

type overlayEntry struct {
	key      string
	validate func(doc []byte) error
}

// overlayTable is each settable overlay key's own parser in help order; its names are vaultdoc.SettableOverlayKeys.
var overlayTable = []overlayEntry{
	{"proxy-teams", func(doc []byte) error {
		_, ps := proxyteam.Parse(assets.ProxyTeams(), doc)
		if len(ps) > 0 {
			return errors.New("invalid proxy-teams document")
		}
		return nil
	}},
	{"providers", providers.Validate},
	{"agent-records", validateAgentRecords},
	{"opencode-models", func(doc []byte) error {
		_, err := providers.ParseOpencodeMenu(doc)
		return err
	}},
}

// vaultOverlaySet is `vault set overlay KEY --file PATH|-`: validate, then store and audit (--dry-run stops before).
func vaultOverlaySet(key, file string, dryRun bool) int {
	if file == "" {
		return helptext.FailUsage("vault set overlay: KEY needs --file PATH|-")
	}
	i := slices.IndexFunc(overlayTable, func(e overlayEntry) bool { return e.key == key })
	if i < 0 {
		return helptext.Fail("vault set overlay: key has no validator; settable keys: %s", strings.Join(vaultdoc.SettableOverlayKeys, ", "))
	}
	raw, err := readOverlayDoc(file)
	if err != nil {
		return helptext.Fail("vault set overlay: %v", err)
	}
	if err := overlayTable[i].validate(raw); err != nil {
		why := "invalid document"
		if key == vault.AgentRecordsKey { // validateAgentRecords errors never echo a value
			why = err.Error()
		}
		return helptext.Fail("vault set overlay: %s refused, nothing written (%s)", key, why)
	}
	if dryRun {
		fmt.Fprintf(os.Stdout, "dry run: overlay %s would be set to %d bytes; nothing written\n", key, len(raw))
		return 0
	}
	old, err := vaultdoc.SetOverlay(key, string(raw))
	if err != nil {
		return helptext.Fail("vault set overlay: %v", err)
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
