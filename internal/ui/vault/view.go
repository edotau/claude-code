package vault

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/theme"
)

// Sections is every `vault list` section, in print order.
var Sections = []string{"secrets", "pins", "env", "agents", "providers", "routing", "settings", "vault"}

// List writes the selected sections to out in format (helptext.JSON or helptext.Text); secrets stay masked.
func List(out *os.File, format string, sel []string) error {
	var stored []secrets.Stored
	if slices.Contains(sel, "secrets") {
		var err error
		if stored, err = secrets.ListStored(nil); err != nil {
			return err
		}
	}
	raw, err := configure.Export()
	if err != nil {
		return err
	}
	if format != helptext.JSON {
		var doc configure.Doc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return err
		}
		_, err := io.WriteString(out, Text(doc, stored, sel, Width(), ansi.Enabled(out)))
		return err
	}
	b, err := JSON(raw, stored, sel)
	if err != nil {
		return err
	}
	_, err = out.Write(b)
	return err
}

// Select maps case-insensitive names (none or "all" = every one) to sections in print order.
func Select(names []string) ([]string, error) {
	want, all := map[string]bool{}, false
	for _, n := range names {
		n = strings.ToLower(n)
		switch {
		case n == "all":
			all = true
		case slices.Contains(Sections, n):
			want[n] = true
		default:
			return nil, fmt.Errorf("unknown section %q (want %s or all)", n, strings.Join(Sections, "|"))
		}
	}
	if all || len(want) == 0 {
		return Sections, nil
	}
	var sel []string
	for _, s := range Sections {
		if want[s] {
			sel = append(sel, s)
		}
	}
	return sel, nil
}

// secretJSON is one masked secret in `vault list --format json`; the value never appears.
type secretJSON struct {
	Name   string `json:"name"`
	Masked string `json:"masked"`
	Length int    `json:"length"`
}

// JSON is configure.Export()'s object, filtered to sel, with a masked "secrets" array first when selected.
func JSON(raw []byte, stored []secrets.Stored, sel []string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	rows := make([]secretJSON, 0, len(stored))
	for _, s := range stored {
		rows = append(rows, secretJSON{Name: s.Name, Masked: s.Masked(), Length: s.Len})
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

// Tailscale writes presence (never a value) per credential name, then the api verdict.
func Tailscale(w io.Writer) {
	cs := tailscale.Credentials()
	for _, c := range cs {
		if c.Present {
			fmt.Fprintf(w, "%s  set (%s)\n", c.Name, c.Source)
		} else {
			fmt.Fprintf(w, "%s  missing\n", c.Name)
		}
	}
	fmt.Fprintln(w, "api:", tailscaleAPIVerdict(cs))
}

// tailscaleAPIVerdict names which admin credential, if any, Credentials (in its fixed order) resolves to.
func tailscaleAPIVerdict(cs []tailscale.CredState) string {
	id, sec, api := cs[0].Present, cs[1].Present, cs[2].Present
	switch {
	case id && sec:
		return "oauth"
	case api: // NewClient falls back to the API key when the OAuth pair is half-set
		return "api-key"
	case id != sec:
		return "incomplete (need both …_ID and _SECRET)"
	default:
		return "none"
	}
}
