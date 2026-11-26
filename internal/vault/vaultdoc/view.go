package vaultdoc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/tailscale"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/table"
	"github.com/edotau/claude-code/internal/ui/theme"
	"github.com/edotau/claude-code/internal/vault"
)

// Sections is every `vault ls` section, in print order.
var Sections = []string{"secrets", "cache", "env", "agents", "providers", "routing", "vault"}

// List writes the selected sections to out in format (helptext.JSON or helptext.Text); secrets stay masked.
func List(out *os.File, format string, sel []string) error {
	var recs Records
	var err error
	if slices.Contains(sel, "secrets") {
		if recs.Secrets, err = vault.ListStored(nil); err != nil {
			return err
		}
	}
	if slices.Contains(sel, "cache") {
		if recs.Cache, err = vault.ListCache(); err != nil {
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
		_, err := io.WriteString(out, Text(doc, recs, sel, Width(), ansi.Enabled(out)))
		return err
	}
	b, err := JSON(raw, recs, sel)
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

// cacheJSON is one snapshot in `vault ls --format json`: name, label, size and set-time, never the value.
type cacheJSON struct {
	Name  string    `json:"name"`
	Label string    `json:"label,omitempty"`
	Size  int       `json:"size"`
	SetAt time.Time `json:"set_at,omitzero"`
}

// Records is the vault records `vault ls` lists: masked secrets and value-free cache snapshots.
type Records struct{ Secrets, Cache []vault.Stored }

// JSON is Export()'s object, filtered to sel, with masked "secrets" and value-free "cache" arrays when selected.
func JSON(raw []byte, recs Records, sel []string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	rows := make([]secretJSON, 0, len(recs.Secrets))
	for _, s := range recs.Secrets {
		rows = append(rows, secretJSON{Name: s.Name, Masked: s.Masked(), Length: s.Len})
	}
	crows := make([]cacheJSON, 0, len(recs.Cache))
	for _, s := range recs.Cache {
		crows = append(crows, cacheJSON{Name: s.Name, Label: s.Label, Size: s.Len, SetAt: s.SetAt})
	}
	var err error
	if doc["secrets"], err = json.Marshal(rows); err != nil {
		return nil, err
	}
	if doc["cache"], err = json.Marshal(crows); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("{")
	for i, s := range sel {
		ind, err := sectionJSON(s, doc)
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

// sectionJSON is section s re-indented for JSON's object: routing derived, absent as null.
func sectionJSON(s string, doc map[string]json.RawMessage) ([]byte, error) {
	val := doc[s]
	switch s {
	case "routing":
		v, err := vaultRoutingJSON(doc["providers"], doc["env"])
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

// cacheAge is the time since a snapshot was sealed (30s, 5m, 3h, 2d), "" when unknown.
func cacheAge(sealed time.Time) string {
	d := time.Since(sealed)
	switch {
	case sealed.IsZero():
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(max(d, 0).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// vaultSecretsText is the SECRETS section, sorted by name: columns justified to the terminal, names clipped mid-way.
func vaultSecretsText(stored []vault.Stored, width int) string {
	if len(stored) == 0 {
		return "SECRETS\n  (none)\n"
	}
	lenW := 0
	for _, s := range stored {
		lenW = max(lenW, len(strconv.Itoa(s.Len)))
	}
	cells := [][]string{}
	for _, s := range stored {
		prefix := s.Prefix
		if strings.HasPrefix(prefix, `{"`) { // a JSON token set: the stored leading bytes say nothing
			prefix = "json"
			if strings.HasSuffix(s.Name, "_OAUTH") {
				prefix = "oauth"
			}
		}
		cells = append(cells, []string{s.Name, prefix + "***", fmt.Sprintf("%*d chars", lenW, s.Len)})
	}
	return "SECRETS\n" + table.Indented(cells, width, table.TruncateMiddle)
}

// vaultCacheText is the CACHE section: name (a trailing sha256 cut to 8), label, size and age, never the value.
func vaultCacheText(cached []vault.Stored, width int) string {
	if len(cached) == 0 {
		return "CACHE\n  (none)\n"
	}
	cells := [][]string{}
	for _, s := range cached {
		name := s.Name
		if i := strings.LastIndex(name, "/"); len(name)-i-1 == 64 {
			name = name[:i+9]
		}
		cells = append(cells, []string{name, s.Label, fmt.Sprintf("%d bytes", s.Len), cacheAge(s.SetAt)})
	}
	return "CACHE\n" + table.Indented(cells, width, table.TruncateMiddle)
}
