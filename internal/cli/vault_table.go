package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/config"
	"github.com/edotau/claude-code/internal/helptext"
)

// vaultShowText renders config.Export() as a human-readable, gradient-painted table.
func vaultShowText() int {
	raw, err := config.Export()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vault show:", err)
		return 1
	}
	var doc config.Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		fmt.Fprintln(os.Stderr, "vault show:", err)
		return 1
	}
	helptext.Fprint(os.Stdout, renderVaultText(doc, vaultTableWidth()))
	return 0
}

// vaultTableWidth is $COLUMNS when set, else 120 (no live TTY-size query exists in this repo).
func vaultTableWidth() int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	return 120
}

// renderVaultText lays out one section per top-level Doc field, uppercase heading + aligned rows.
func renderVaultText(doc config.Doc, width int) string {
	sections := []struct {
		head string
		rows [][2]string
	}{
		{"PINS", vaultMapRows(doc.Pins)},
		{"ENV", vaultMapRows(doc.Env)},
		{"PROVIDERS", vaultFlattenSection(doc.Providers, "providers")},
		{"VENDORS", vaultFlattenSection(doc.Vendors, "vendors")},
		{"SETTINGS", vaultFlattenSection(doc.Settings, "")},
		{"VAULT", vaultVaultRows(doc.Vault.KeyCommand)},
	}
	var b strings.Builder
	for i, s := range sections {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(vaultSection(s.head, s.rows, width))
	}
	return b.String()
}

// vaultMapRows sorts a pins/env map into rows by key.
func vaultMapRows(m map[string]string) [][2]string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([][2]string, len(keys))
	for i, k := range keys {
		rows[i] = [2]string{k, m[k]}
	}
	return rows
}

// vaultVaultRows is the single vault.key_command row.
func vaultVaultRows(keyCommand string) [][2]string {
	if keyCommand == "" {
		keyCommand = "(none)"
	}
	return [][2]string{{"key_command", keyCommand}}
}

// vaultFlattenSection flattens an overlay to dotted-path rows, dropping a providers.providers-style wrapper.
func vaultFlattenSection(raw json.RawMessage, wrapper string) [][2]string {
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		return nil
	}
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return [][2]string{{"error", err.Error()}}
	}
	if m, ok := top.(map[string]any); ok {
		if inner, ok := m[wrapper].(map[string]any); wrapper != "" && ok {
			merged := maps.Clone(m)
			delete(merged, wrapper)
			maps.Copy(merged, inner)
			top = merged
		}
	}
	nullLabel := "null"
	if wrapper == "providers" {
		nullLabel = "null (removed)" // Registry.Providers[name]=null means "remove this built-in"
	}
	var kvs []vaultKV
	flattenVaultValue("", top, nullLabel, &kvs)
	rows := make([][2]string, len(kvs))
	for i, kv := range kvs {
		rows[i] = [2]string{kv.path, kv.value}
	}
	return rows
}

// vaultKV is one flattened dotted-path/value pair.
type vaultKV struct{ path, value string }

// flattenVaultValue walks a decoded JSON value in sorted-key order (arrays keep index order), one vaultKV per leaf.
func flattenVaultValue(prefix string, v any, nullLabel string, out *[]vaultKV) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			vv, p := t[k], k
			if prefix != "" {
				p = prefix + "." + k
			}
			flattenVaultValue(p, vv, nullLabel, out)
		}
	case []any:
		for i, vv := range t {
			flattenVaultValue(fmt.Sprintf("%s[%d]", prefix, i), vv, nullLabel, out)
		}
	case nil:
		*out = append(*out, vaultKV{prefix, nullLabel})
	case string:
		*out = append(*out, vaultKV{prefix, t})
	case bool:
		*out = append(*out, vaultKV{prefix, strconv.FormatBool(t)})
	case float64:
		*out = append(*out, vaultKV{prefix, strconv.FormatFloat(t, 'g', -1, 64)})
	default:
		*out = append(*out, vaultKV{prefix, fmt.Sprint(t)})
	}
}

// vaultSection renders one heading plus its aligned, width-truncated rows (or "(none)" when empty).
func vaultSection(heading string, rows [][2]string, width int) string {
	var b strings.Builder
	b.WriteString(heading + "\n")
	if len(rows) == 0 {
		b.WriteString("  (none)\n")
		return b.String()
	}
	keyW := 0
	for _, r := range rows {
		if n := len(r[0]); n > keyW {
			keyW = n
		}
	}
	for _, r := range rows {
		line := fmt.Sprintf("  %-*s  %s", keyW, r[0], r[1])
		b.WriteString(vaultTruncate(line, width))
		b.WriteString("\n")
	}
	return b.String()
}

// vaultTruncate clips a plain (unpainted) line to width runes, ending in "…".
func vaultTruncate(s string, width int) string {
	r := []rune(s)
	if width <= 0 || len(r) <= width {
		return s
	}
	if width <= 1 {
		return string(r[:width])
	}
	return string(r[:width-1]) + "…"
}
