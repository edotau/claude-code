package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/theme"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// vaultSections is every `vault list` section, in print order.
var vaultSections = []string{"secrets", "pins", "env", "agents", "providers", "routing", "settings", "vault"}

// vaultList prints the selected sections (default all): secrets masked, then the vault's non-secret config.
func vaultList(args []string) int {
	fs := helptext.NewFlagSet("vault list", helptext.Page{
		Usage: "claude-code vault list [<section>...|all]\n[--format text|json] [--tailscale]",
		About: "Print what the vault stores, secrets masked. Sections: " +
			strings.Join(vaultSections, ", ") + "; bare or all prints every one.",
	})
	ts := fs.Bool("tailscale", false, "show Tailscale credential presence, not JSON")
	format := fs.String("format", "", "text or json (default: text on a terminal, json otherwise)")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *ts {
		if len(names) > 0 {
			return vaultUsageErr("list", names)
		}
		return vaultListTailscale()
	}
	sel, err := vaultSelectSections(names)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vault list:", err)
		return 2
	}
	f := *format
	if f == "" {
		f = helptext.JSON
		if tui.StdoutIsTerminal() {
			f = helptext.Text
		}
	}
	if err := helptext.CheckFormat(f); err != nil {
		fmt.Fprintln(os.Stderr, "vault list:", err)
		return 2
	}
	var stored []secrets.Stored
	if slices.Contains(sel, "secrets") {
		if stored, err = secrets.ListStored(nil); err != nil {
			return fail("vault list: %v", err)
		}
	}
	raw, err := configure.Export()
	if err != nil {
		return fail("vault list: %v", err)
	}
	var out []byte
	if f == helptext.JSON {
		out, err = vaultListJSON(raw, stored, sel)
	} else {
		var doc configure.Doc
		if err = json.Unmarshal(raw, &doc); err == nil {
			fmt.Print(renderVaultTextStyled(doc, stored, sel, vaultTableWidth(), ansi.Enabled(os.Stdout)))
			return 0
		}
	}
	if err != nil {
		return fail("vault list: %v", err)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		return fail("vault list: %v", err)
	}
	return 0
}

// vaultSelectSections maps case-insensitive names (none or "all" = every one) to sections in print order.
func vaultSelectSections(names []string) ([]string, error) {
	want, all := map[string]bool{}, false
	for _, n := range names {
		n = strings.ToLower(n)
		switch {
		case n == "all":
			all = true
		case slices.Contains(vaultSections, n):
			want[n] = true
		default:
			return nil, fmt.Errorf("unknown section %q (want %s or all)", n, strings.Join(vaultSections, "|"))
		}
	}
	if all || len(want) == 0 {
		return vaultSections, nil
	}
	var sel []string
	for _, s := range vaultSections {
		if want[s] {
			sel = append(sel, s)
		}
	}
	return sel, nil
}

// vaultSecretJSON is one masked secret in `vault list --format json`; the value never appears.
type vaultSecretJSON struct {
	Name   string `json:"name"`
	Masked string `json:"masked"`
	Length int    `json:"length"`
}

// vaultListJSON is configure.Export()'s object, filtered to sel, with a masked "secrets" array first when selected.
func vaultListJSON(raw []byte, stored []secrets.Stored, sel []string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	rows := make([]vaultSecretJSON, 0, len(stored))
	for _, s := range stored {
		rows = append(rows, vaultSecretJSON{Name: s.Name, Masked: s.Masked(), Length: s.Len})
	}
	var b bytes.Buffer
	b.WriteString("{")
	for i, s := range sel {
		val := doc[s]
		switch s {
		case "secrets":
			v, err := json.Marshal(rows)
			if err != nil {
				return nil, err
			}
			val = v
		case "routing":
			v, err := vaultRoutingJSON(doc["providers"])
			if err != nil {
				return nil, err
			}
			val = v
		}
		if len(val) == 0 {
			val = json.RawMessage("null")
		}
		var compact, ind bytes.Buffer
		if err := json.Compact(&compact, val); err != nil {
			return nil, err
		}
		if err := json.Indent(&ind, compact.Bytes(), "  ", "  "); err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n  %q: %s", s, ind.Bytes())
	}
	b.WriteString("\n}\n")
	return b.Bytes(), nil
}

// paintMarks colors the ✓ (verified) and ✗ (unverified) env marks green and red.
func paintMarks(s string) string {
	mode, _, _ := theme.Theme()
	s = strings.ReplaceAll(s, "✓", ansi.Paint(mode, ansi.RGB{80, 200, 120}, "✓", true))
	return strings.ReplaceAll(s, "✗", ansi.Paint(mode, ansi.RGB{230, 90, 90}, "✗", true))
}
