package vault

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/ui/table"
	"github.com/edotau/claude-code/internal/vault"
)

const secretNameMax = 40

// recordLabels is every record's plaintext label by name; read without a key, so nothing is decrypted (a test seam).
var recordLabels = func() map[string]string {
	out := map[string]string{}
	_ = vault.View(func(tx *vault.Tx) error {
		recs, err := tx.Records()
		for n, r := range recs {
			if r.Label != "" {
				out[n] = r.Label
			}
		}
		return err
	})
	return out
}

// secretRow is one SECRETS line: display name, masked preview, and the trailing "N chars" or discovery suffix.
type secretRow struct{ name, mask, tail string }

// discoveryName is "<PROVIDER>_<PROFILE>_MODEL_DISCOVERY", or the refresh hint when the record is unlabeled.
func discoveryName(label string) string {
	if label == "" {
		return "MODEL_DISCOVERY (refresh to label)"
	}
	up := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, strings.ToUpper(label))
	return up + "_MODEL_DISCOVERY"
}

// secretRows builds the display rows, sorted by display name.
func secretRows(stored []secrets.Stored, labels map[string]string) []secretRow {
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
			kr[i].secretRow = secretRow{discoveryName(labels[s.Name]), "mode***", "/v1/" + hash[:min(12, len(hash))]}
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

// vaultSecretsText is the SECRETS section: name column fitted to the section (capped), one clipped line per row.
func vaultSecretsText(stored []secrets.Stored, width int) string {
	if len(stored) == 0 {
		return "SECRETS\n  (none)\n"
	}
	var labels map[string]string // read only when a discovery record needs its label
	if slices.ContainsFunc(stored, func(s secrets.Stored) bool { return strings.HasPrefix(s.Name, serving.DiscoveryKeyPrefix) }) {
		labels = recordLabels()
	}
	rows := secretRows(stored, labels)
	nameW, maskW := 0, 0
	for i, r := range rows {
		rows[i].name = table.Truncate(r.name, secretNameMax)
		nameW = max(nameW, utf8.RuneCountInString(rows[i].name))
		maskW = max(maskW, utf8.RuneCountInString(r.mask))
	}
	var b strings.Builder
	b.WriteString("SECRETS\n")
	for _, r := range rows {
		line := fmt.Sprintf("  %-*s  %-*s  %s", nameW, r.name, maskW, r.mask, r.tail)
		b.WriteString(table.Truncate(line, width))
		b.WriteByte('\n')
	}
	return b.String()
}
