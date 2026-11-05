package vaultcmd

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/ui/table"
	"github.com/edotau/claude-code/internal/vault"
)

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
		fmt.Fprintln(os.Stderr, "vault list: reading labels:", err)
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

// discoveryAge is "  <age>" (30s, 5m, 3h, 2d) since a snapshot was sealed (its discovery time), "" when unknown; fits 80 columns.
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
