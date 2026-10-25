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

	"unicode/utf8"

	"github.com/edotau/claude-code/internal/config"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
)

// vaultTableWidth is $COLUMNS when set, else 120 (no live TTY-size query exists in this repo).
func vaultTableWidth() int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	return 120
}

// renderVaultText lays out each selected section, uppercase heading + aligned rows, in vaultSections order.
func renderVaultText(doc config.Doc, stored []secrets.Stored, sel []string, width int) string {
	render := map[string]func() string{
		"secrets":   func() string { return vaultSection("SECRETS", vaultSecretRows(stored), width) },
		"pins":      func() string { return vaultSection("PINS", vaultMapRows(doc.Pins), width) },
		"env":       func() string { return vaultSection("ENV", vaultMapRows(doc.Env), width) },
		"providers": func() string { return vaultProvidersText(doc.Providers, width) },
		"vendors":   func() string { return vaultVendorsText(doc.Vendors, width) },
		"settings":  func() string { return vaultSection("SETTINGS", vaultFlattenSection(doc.Settings), width) },
		"vault":     func() string { return vaultSection("VAULT", vaultVaultRows(doc.Vault.KeyCommand), width) },
	}
	var b strings.Builder
	for i, s := range sel {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(render[s]())
	}
	return b.String()
}

// vaultSecretRows is one name/masked row per stored secret; nothing is decrypted.
func vaultSecretRows(stored []secrets.Stored) [][2]string {
	rows := make([][2]string, len(stored))
	for i, s := range stored {
		rows[i] = [2]string{s.Name, s.Masked()}
	}
	return rows
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

// vaultFlattenSection flattens an overlay to dotted-path rows.
func vaultFlattenSection(raw json.RawMessage) [][2]string {
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		return nil
	}
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return [][2]string{{"error", err.Error()}}
	}
	return vaultFlattenValue("", top)
}

// vaultFlattenValue flattens one decoded value under prefix to path/value rows.
func vaultFlattenValue(prefix string, v any) [][2]string {
	var kvs []vaultKV
	flattenVaultValue(prefix, v, "null", &kvs)
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

// vaultProvidersText renders the providers overlay as a provider table, a PLANS table and one-line lists.
func vaultProvidersText(raw json.RawMessage, width int) string {
	var b strings.Builder
	b.WriteString("PROVIDERS\n")
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		b.WriteString("  (none)\n")
		return b.String()
	}
	var reg providers.Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		b.WriteString(vaultTruncate("  error  "+err.Error(), width) + "\n")
		return b.String()
	}
	var blocks []string
	if len(reg.Providers) > 0 {
		var rows [][]string
		for _, name := range slices.Sorted(maps.Keys(reg.Providers)) {
			p := reg.Providers[name]
			if p == nil { // "name": null removes a built-in
				rows = append(rows, []string{name, "(removed)", "", "", "", "", "", ""})
				continue
			}
			rows = append(rows, []string{name, p.Kind, p.Auth.Type, p.Models["opus"], p.Models["sonnet"],
				p.Models["haiku"], p.OpencodeID, p.OpencodeFallback})
		}
		head := []string{"PROVIDER", "KIND", "AUTH", "OPUS", "SONNET", "HAIKU", "OPENCODE", "FALLBACK"}
		blocks = append(blocks, vaultTable(head, rows, width))
	}
	if len(reg.AgentPlans) > 0 {
		var rows [][]string
		for _, agent := range slices.Sorted(maps.Keys(reg.AgentPlans)) {
			for _, p := range reg.AgentPlans[agent] {
				rows = append(rows, []string{agent, p.Tier, p.ID})
			}
		}
		blocks = append(blocks, "  PLANS\n"+vaultTable([]string{"AGENT", "TIER", "ID"}, rows, width))
	}
	var lines [][2]string
	if reg.Default != "" {
		lines = append(lines, [2]string{"default", reg.Default})
	}
	if len(reg.Fallback) > 0 {
		lines = append(lines, [2]string{"fallback", strings.Join(reg.Fallback, " → ")})
	}
	if len(reg.OpencodeLadder) > 0 {
		lines = append(lines, [2]string{"opencode ladder", strings.Join(reg.OpencodeLadder, " → ")})
	}
	if len(reg.OpencodePlugins) > 0 {
		lines = append(lines, [2]string{"opencode plugins", strings.Join(reg.OpencodePlugins, ", ")})
	}
	if reg.AgentPlanFormat != "" {
		lines = append(lines, [2]string{"agent plan format", reg.AgentPlanFormat})
	}
	if len(lines) > 0 {
		blocks = append(blocks, strings.TrimPrefix(vaultSection("", lines, width), "\n"))
	}
	if len(blocks) == 0 {
		b.WriteString("  (none)\n")
	}
	b.WriteString(strings.Join(blocks, "\n"))
	return b.String()
}

// vaultTable renders a header plus rows as two-space-indented, column-aligned, width-truncated lines.
func vaultTable(head []string, rows [][]string, width int) string {
	ws := make([]int, len(head))
	for _, r := range append([][]string{head}, rows...) {
		for i, c := range r {
			ws[i] = max(ws[i], utf8.RuneCountInString(c))
		}
	}
	var b strings.Builder
	for _, r := range append([][]string{head}, rows...) {
		line := " "
		var lineSb242 strings.Builder
		for i, c := range r {
			lineSb242.WriteString(" " + c)
			if i < len(r)-1 {
				lineSb242.WriteString(strings.Repeat(" ", ws[i]-utf8.RuneCountInString(c)+1))
			}
		}
		line += lineSb242.String()
		b.WriteString(vaultTruncate(strings.TrimRight(line, " "), width) + "\n")
	}
	return b.String()
}

// vaultVendorsText renders the vendors overlay as one block per vendor; unknown keys fall back to the flatten.
func vaultVendorsText(raw json.RawMessage, width int) string {
	var b strings.Builder
	b.WriteString("VENDORS\n")
	var top map[string]any
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		b.WriteString("  (none)\n")
		return b.String()
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return vaultSection("VENDORS", vaultFlattenSection(raw), width)
	}
	var blocks []string
	vs, _ := top["vendors"].(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(vs)) {
		rows := vaultVendorRows(vs[name])
		blocks = append(blocks, "  "+name+"\n"+strings.TrimPrefix(vaultIndent(vaultSection("", rows, width-2)), "\n"))
	}
	var rest [][2]string
	for _, k := range slices.Sorted(maps.Keys(top)) {
		if k == "_comment" || (k == "vendors" && vs != nil) {
			continue
		}
		rest = append(rest, vaultFlattenValue(k, top[k])...)
	}
	if len(rest) > 0 {
		blocks = append(blocks, strings.TrimPrefix(vaultSection("", rest, width), "\n"))
	}
	if len(blocks) == 0 {
		b.WriteString("  (none)\n")
	}
	b.WriteString(strings.Join(blocks, "\n"))
	return b.String()
}

// vaultVendorRows renders one vendor entry: launch.env, launch.args, permission and mcp.allow read as lists.
func vaultVendorRows(v any) [][2]string {
	m, ok := v.(map[string]any)
	if !ok {
		if v == nil {
			return [][2]string{{"(removed)", ""}}
		}
		return vaultFlattenValue("", v)
	}
	var rows [][2]string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		switch sub, isMap := m[k].(map[string]any); {
		case k == "_comment":
		case k == "permission":
			var parts []string
			for _, r := range vaultFlattenValue("", m[k]) {
				parts = append(parts, r[0]+"="+r[1])
			}
			rows = append(rows, [2]string{k, strings.Join(parts, ", ")})
		case (k == "launch" || k == "mcp") && isMap:
			for _, sk := range slices.Sorted(maps.Keys(sub)) {
				path, sep := k+"."+sk, map[string]string{"launch.env": ", ", "launch.args": " ", "mcp.allow": ", "}[k+"."+sk]
				if s, ok := vaultStringList(sub[sk]); ok && sep != "" {
					rows = append(rows, [2]string{path, strings.Join(s, sep)})
					continue
				}
				rows = append(rows, vaultFlattenValue(path, sub[sk])...)
			}
		default:
			rows = append(rows, vaultFlattenValue(k, m[k])...)
		}
	}
	return rows
}

// vaultStringList reports v as a []string when it is a JSON array of strings.
func vaultStringList(v any) ([]string, bool) {
	a, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(a))
	for _, e := range a {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// vaultIndent adds two spaces before every non-empty line.
func vaultIndent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "  " + l
		}
	}
	return strings.Join(lines, "\n")
}
