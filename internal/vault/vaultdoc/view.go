package vaultdoc

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/table"
	"github.com/edotau/claude-code/internal/ui/theme"
	"github.com/edotau/claude-code/internal/vault"
)

// Sections is every `vault ls` section, in print order.
var Sections = []string{"secrets", "pins", "env", "agents", "providers", "routing", "settings", "vault"}

// List writes the selected sections to out in format (helptext.JSON or helptext.Text); secrets stay masked.
func List(out *os.File, format string, sel []string) error {
	var stored []vault.Stored
	if slices.Contains(sel, "secrets") {
		var err error
		if stored, err = vault.ListStored(nil); err != nil {
			return err
		}
	}
	raw, err := Export()
	if err != nil {
		return err
	}
	if format != helptext.JSON {
		var doc Doc
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

// secretJSON is one masked secret in `vault ls --format json`; the value never appears.
type secretJSON struct {
	Name   string `json:"name"`
	Masked string `json:"masked"`
	Length int    `json:"length"`
}

// JSON is Export()'s object, filtered to sel, with a masked "secrets" array first when selected.
func JSON(raw []byte, stored []vault.Stored, sel []string) ([]byte, error) {
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
		ind, err := sectionJSON(s, doc, rows)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n  %q: %s", s, ind)
	}
	b.WriteString("\n}\n")
	return b.Bytes(), nil
}

// sectionJSON is section s re-indented for JSON's object: secrets masked, routing derived, absent as null.
func sectionJSON(s string, doc map[string]json.RawMessage, rows []secretJSON) ([]byte, error) {
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
	return ind.Bytes(), nil
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

// recordLabels is every record's plaintext label by name; read without a key, so nothing is decrypted (a test seam).
var recordLabels = func() map[string]string {
	out := map[string]string{}
	err := vault.View(func(tx *vault.Tx) error {
		recs, err := tx.Records()
		for n, r := range recs {
			if r.Label != "" {
				out[n] = r.Label
			}
		}
		return err
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "vault ls: reading labels:", err)
	}
	return out
}

// secretRow is one SECRETS line: display name, masked preview, and the trailing "N chars" or discovery suffix.
type secretRow struct{ name, mask, tail string }

// discoveryName is "<PROVIDER>_<PROFILE>_<kind>", or the refresh hint when the record is unlabeled.
func discoveryName(label, kind string) string {
	if label == "" {
		return kind + " (refresh to label)"
	}
	up := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, strings.ToUpper(label))
	return up + "_" + kind
}

// discoveryAge is "  <age>" (30s, 5m, 3h, 2d) since a snapshot was sealed, "" when unknown; fits 80 columns.
func discoveryAge(sealed time.Time) string {
	d := time.Since(sealed)
	switch {
	case sealed.IsZero():
		return ""
	case d < time.Minute:
		return fmt.Sprintf("  %ds", int(max(d, 0).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("  %dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("  %dh", int(d.Hours()))
	}
	return fmt.Sprintf("  %dd", int(d.Hours()/24))
}

// secretRows builds the display rows, sorted by display name.
func secretRows(stored []vault.Stored, labels map[string]string) []secretRow {
	lenW := 0
	for _, s := range stored {
		lenW = max(lenW, len(strconv.Itoa(s.Len)))
	}
	type keyed struct {
		secretRow
		key string
	}
	kr := make([]keyed, len(stored))
	for i, s := range stored {
		kr[i].key = s.Name
		if hash, ok := strings.CutPrefix(s.Name, serving.DiscoveryKeyPrefix); ok {
			kr[i].secretRow = secretRow{discoveryName(labels[s.Name], "MODEL_DISCOVERY"), "mode***",
				"/v1/" + hash[:min(8, len(hash))] + discoveryAge(s.SetAt)}
			continue
		}
		if hash, ok := strings.CutPrefix(s.Name, serving.SubjectKeyPrefix); ok {
			kr[i].secretRow = secretRow{discoveryName(labels[s.Name], "MODEL_SUBJECT"), "id***", "/v1/" + hash[:min(8, len(hash))]}
			continue
		}
		prefix := s.Prefix
		if strings.HasPrefix(prefix, `{"`) { // a JSON token set: the stored leading bytes say nothing
			prefix = "json"
			if strings.HasSuffix(s.Name, "_OAUTH") {
				prefix = "oauth"
			}
		}
		kr[i].secretRow = secretRow{s.Name, prefix + "***", fmt.Sprintf("%*d chars", lenW, s.Len)}
	}
	slices.SortFunc(kr, func(a, b keyed) int {
		return cmp.Or(cmp.Compare(a.name, b.name), cmp.Compare(a.key, b.key))
	})
	rows := make([]secretRow, len(kr))
	for i, k := range kr {
		rows[i] = k.secretRow
	}
	return rows
}

// vaultSecretsText is the SECRETS section: columns justified to the terminal, names clipped in the middle.
func vaultSecretsText(stored []vault.Stored, width int) string {
	if len(stored) == 0 {
		return "SECRETS\n  (none)\n"
	}
	var labels map[string]string // read only when a discovery record needs its label
	if slices.ContainsFunc(stored, func(s vault.Stored) bool {
		return strings.HasPrefix(s.Name, serving.DiscoveryKeyPrefix) || strings.HasPrefix(s.Name, serving.SubjectKeyPrefix)
	}) {
		labels = recordLabels()
	}
	cells := [][]string{}
	for _, r := range secretRows(stored, labels) {
		cells = append(cells, []string{r.name, r.mask, r.tail})
	}
	return "SECRETS\n" + table.Indented(cells, width, table.TruncateMiddle)
}
