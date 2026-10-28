package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
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
		"secrets":   func() string { return vaultSectionMax("SECRETS", vaultSecretRows(stored), width, 0, 0) },
		"pins":      func() string { return vaultSection("PINS", vaultMapRows(doc.Pins), width) },
		"env":       func() string { return vaultSection("ENV", vaultMapRows(doc.Env), width) },
		"providers": func() string { return vaultProvidersText(doc.Providers, width) },
		"routing":   func() string { return vaultRoutingText(doc.Providers, width) },
		"agents":    func() string { return vaultVendorsText(doc.Vendors, doc.Pins, width) },
		"settings":  func() string { return vaultSection("SETTINGS", vaultFlattenSection(doc.Settings), width) },
		"vault":     func() string { return vaultSection("VAULT", vaultVaultRows(doc.Vault.KeyCommand), width) },
	}
	var b strings.Builder
	for _, s := range sel {
		out := render[s]()
		if len(sel) == len(vaultSections) && emptySection(out) { // bare/all hides empty sections; named ones say (none)
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(out)
	}
	if b.Len() == 0 {
		return "(vault is empty)\n"
	}
	return b.String()
}

// emptySection reports a rendered section that is only its heading plus "(none)".
func emptySection(out string) bool {
	_, body, _ := strings.Cut(out, "\n")
	return body == "  (none)\n"
}

// vaultSecretRows is one name/"<prefix>***  N chars" row per secret; preview and right-aligned length are columns.
func vaultSecretRows(stored []secrets.Stored) [][2]string {
	prefixW, lenW := 0, 0
	for _, s := range stored {
		prefixW = max(prefixW, len(s.Prefix)+3)
		lenW = max(lenW, len(strconv.Itoa(s.Len)))
	}
	rows := make([][2]string, len(stored))
	for i, s := range stored {
		rows[i] = [2]string{s.Name, fmt.Sprintf("%-*s  %*d chars", prefixW, s.Prefix+"***", lenW, s.Len)}
	}
	return rows
}

// vaultMapRows sorts a pins/env map into rows by key.
func vaultMapRows(m map[string]string) [][2]string {
	keys := slices.Sorted(maps.Keys(m))
	rows := make([][2]string, len(keys))
	for i, k := range keys {
		rows[i] = [2]string{k, m[k]}
	}
	return rows
}

// vaultVaultRows is the vault.key_command row; none when unset (the OS keyring or HARNESS_VAULT_KEY supplies the key).
func vaultVaultRows(keyCommand string) [][2]string {
	if keyCommand == "" {
		return nil
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
	return vaultSectionMax(heading, rows, width, kvKeyMax, kvValueMax)
}

// Key/value sections (PINS, ENV, ROUTING, SETTINGS, AGENTS, VAULT) clip a longer key or value with "…".
const (
	kvKeyMax   = 24
	kvValueMax = 48
)

// vaultSectionMax is vaultSection with explicit key/value caps (0 = no cap); SECRETS lays out its own columns.
func vaultSectionMax(heading string, rows [][2]string, width, keyMax, valueMax int) string {
	rows = slices.Clone(rows)
	for i, r := range rows {
		if keyMax > 0 {
			r[0] = vaultTruncateMiddle(r[0], keyMax) // keys differ at the end ("…Markers[0]" vs "…Markers[1]")
		}
		if valueMax > 0 {
			r[1] = vaultTruncate(r[1], valueMax)
		}
		rows[i] = r
	}
	var b strings.Builder
	b.WriteString(heading)
	b.WriteByte('\n')
	if len(rows) == 0 {
		b.WriteString("  (none)\n")
		return b.String()
	}
	keyW := 0
	for _, r := range rows {
		if n := utf8.RuneCountInString(r[0]); n > keyW {
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

// vaultTruncateMiddle clips s to width runes keeping both ends, so names that share a long prefix stay distinct.
func vaultTruncateMiddle(s string, width int) string {
	r := []rune(s)
	if width <= 2 || len(r) <= width {
		return vaultTruncate(s, width)
	}
	tail := (width - 1) / 2
	return string(r[:width-1-tail]) + "…" + string(r[len(r)-tail:])
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

// vaultProvidersText renders the providers overlay as a provider table (PLANS stays JSON-only) and one-line lists.
func vaultProvidersText(raw json.RawMessage, width int) string {
	var b strings.Builder
	b.WriteString("PROVIDERS\n")
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		b.WriteString("  (none)\n")
		return b.String()
	}
	var reg providers.Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		b.WriteString(vaultTruncate("  error  "+err.Error(), width))
		b.WriteByte('\n')
		return b.String()
	}
	var nested map[string]json.RawMessage
	var top struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	_ = json.Unmarshal(raw, &top)
	nested = top.Providers
	var blocks []string
	if len(nested) > 0 {
		var rows [][]string
		presets := map[string][]string{} // preset → its env rows, shown as one row
		presetOwner := map[string]string{}
		for _, prov := range slices.Sorted(maps.Keys(nested)) {
			if providers.IsNull(nested[prov]) { // "provider": null removes its built-ins
				rows = append(rows, []string{prov, prov, "(removed)", "", "", "", "", ""})
				continue
			}
			var profiles map[string]*providers.Provider
			if err := json.Unmarshal(nested[prov], &profiles); err != nil {
				rows = append(rows, []string{prov, prov, "error: " + err.Error(), "", "", "", "", ""})
				continue
			}
			for _, name := range slices.Sorted(maps.Keys(profiles)) {
				p := profiles[name]
				if p == nil { // "name": null removes a built-in
					rows = append(rows, []string{name, prov, "(removed)", "", "", "", "", ""})
					continue
				}
				if p.Preset != "" {
					presets[p.Preset] = append(presets[p.Preset], name)
					presetOwner[p.Preset] = prov
					continue
				}
				rows = append(rows, []string{name, prov, p.Auth.Method(), p.Models["opus"], p.Models["sonnet"],
					p.Models["haiku"], p.OpencodeID, p.OpencodeFallback})
			}
		}
		for _, preset := range slices.Sorted(maps.Keys(presets)) {
			rows = append(rows, []string{preset, presetOwner[preset], presetEnvs(presets[preset]), "", "", "", "", ""})
		}
		head := []string{"NAME", "PROVIDER", "AUTH", "OPUS", "SONNET", "HAIKU", "OPENCODE", "FALLBACK"}
		if rows = vaultShownRows(rows); len(rows) > 0 {
			head, cols, rows := vaultDropEmptyCols(head, providerColMax, rows)
			blocks = append(blocks, vaultTable(head, cols, rows, width))
		}
	}
	var lines [][2]string
	if reg.Default != "" {
		lines = append(lines, [2]string{"default", reg.Default})
	}
	if len(reg.Fallback) > 0 {
		lines = append(lines, [2]string{"fallback", strings.Join(reg.Fallback, " → ")})
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

// vaultShownRows drops rows with no field past NAME/PROVIDER, and all rows when only removal markers remain.
func vaultShownRows(rows [][]string) [][]string {
	var out [][]string
	live := false
	for _, r := range rows {
		if !slices.ContainsFunc(r[2:], func(c string) bool { return c != "" }) {
			continue
		}
		out = append(out, r)
		live = live || r[2] != "(removed)"
	}
	if !live {
		return nil
	}
	return out
}

// vaultDropEmptyCols removes every column past NAME that is empty in all rows, trimming head and colMax with it.
func vaultDropEmptyCols(head []string, colMax []int, rows [][]string) ([]string, []int, [][]string) {
	var keep []int
	for c := range head {
		if c == 0 || slices.ContainsFunc(rows, func(r []string) bool { return r[c] != "" }) {
			keep = append(keep, c)
		}
	}
	pick := func(r []string) []string {
		out := make([]string, len(keep))
		for i, c := range keep {
			out[i] = r[c]
		}
		return out
	}
	cols := make([]int, len(keep))
	outRows := make([][]string, len(rows))
	for i, c := range keep {
		cols[i] = colMax[c]
	}
	for i, r := range rows {
		outRows[i] = pick(r)
	}
	return pick(head), cols, outRows
}

// vaultCredOK reports whether provider name's credential resolves in the merged registry (a seam for tests).
var vaultCredOK = func(name string) bool {
	reg, err := providers.Load()
	if err != nil {
		return false
	}
	p, ok := reg.Providers[name]
	return ok && providers.CredentialPresent(p)
}

// dbxEnvOrder is the workspace order of a preset's env rows.
var dbxEnvOrder = []string{"dev", "qa", "prod"}

// presetEnvs lists a preset's env rows in dbxEnvOrder, each marked ✓ (credential resolves) or ✗.
func presetEnvs(names []string) string {
	ok := map[string]bool{}
	for _, n := range names {
		ok[n] = vaultCredOK(n)
	}
	slices.SortFunc(names, func(a, b string) int { // failures first so a clipped cell still shows them
		if ok[a] != ok[b] {
			return cmp.Compare(boolRank(ok[a]), boolRank(ok[b]))
		}
		return cmp.Compare(envRank(a), envRank(b))
	})
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = "✗" + n // the mark leads so a clipped cell keeps it
		if ok[n] {
			parts[i] = "✓" + n
		}
	}
	return strings.Join(parts, " ")
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// envRank orders dev/qa/prod first, in that order; other names after.
func envRank(name string) int {
	if i := slices.Index(dbxEnvOrder, name); i >= 0 {
		return i
	}
	return len(dbxEnvOrder)
}

// vaultRoutingText renders the providers overlay's nested routing object (weights, cells, subagents,
// opencode's sub-pick knobs) as dotted-path rows, in the style of vaultProvidersText's own flatten.
func vaultRoutingText(raw json.RawMessage, width int) string {
	var b strings.Builder
	b.WriteString("ROUTING\n")
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		b.WriteString("  (none)\n")
		return b.String()
	}
	var reg providers.Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		b.WriteString(vaultTruncate("  error  "+err.Error(), width))
		b.WriteByte('\n')
		return b.String()
	}
	rb, err := json.Marshal(reg.Routing)
	if err != nil {
		b.WriteString(vaultTruncate("  error  "+err.Error(), width))
		b.WriteByte('\n')
		return b.String()
	}
	rows := vaultFlattenSection(rb)
	if len(rows) == 0 {
		b.WriteString("  (none)\n")
		return b.String()
	}
	b.WriteString(strings.TrimPrefix(vaultSection("", rows, width), "\n"))
	return b.String()
}

// vaultRoutingJSON is `vault list --format json`'s "routing" value: the providers overlay's nested
// routing object, or null when the overlay carries none.
func vaultRoutingJSON(providersRaw json.RawMessage) (json.RawMessage, error) {
	if s := strings.TrimSpace(string(providersRaw)); s == "" || s == "null" {
		return json.RawMessage("null"), nil
	}
	var reg providers.Registry
	if err := json.Unmarshal(providersRaw, &reg); err != nil {
		return nil, err
	}
	return json.Marshal(reg.Routing)
}

// Per-column max widths for the PROVIDERS table; a longer cell is clipped with "…" (0 = no cap).
var (
	providerColMax = []int{20, 10, 20, 14, 14, 16, 16, 18}
)

// vaultTable renders a header plus rows as two-space-indented, column-aligned, width-truncated lines.
func vaultTable(head []string, colMax []int, rows [][]string, width int) string {
	rows = slices.Clone(rows)
	for i, r := range rows {
		r = slices.Clone(r)
		for c := range r {
			if c < len(colMax) && colMax[c] > 0 {
				r[c] = vaultTruncate(r[c], colMax[c])
			}
		}
		rows[i] = r
	}
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
			lineSb242.WriteByte(' ')
			lineSb242.WriteString(c)
			if i < len(r)-1 {
				lineSb242.WriteString(strings.Repeat(" ", ws[i]-utf8.RuneCountInString(c)+1))
			}
		}
		line += lineSb242.String()
		b.WriteString(vaultTruncate(strings.TrimRight(line, " "), width))
		b.WriteByte('\n')
	}
	return b.String()
}

// vaultVendorsText renders one block per agent, ACTIVE first, then its overlay; unknown keys fall back to the flatten.
func vaultVendorsText(raw json.RawMessage, pins map[string]string, width int) string {
	var b strings.Builder
	b.WriteString("AGENTS\n")
	var top map[string]any
	if s := strings.TrimSpace(string(raw)); s != "" && s != "null" {
		if err := json.Unmarshal(raw, &top); err != nil {
			return vaultSection("AGENTS", vaultFlattenSection(raw), width)
		}
	}
	vs, _ := top["agents"].(map[string]any)
	names := []string{"claude", "codex", "copilot", "gemini", "opencode"}
	for _, name := range slices.Sorted(maps.Keys(vs)) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	var blocks []string
	for _, name := range names {
		var rows [][2]string
		if key := providers.AgentActivePin(name); key != "" || name == "claude" {
			rows = append(rows, [2]string{"ACTIVE", vaultAgentActive(key, pins)})
		}
		if v, ok := vs[name]; ok {
			rows = append(rows, vaultVendorRows(v)...)
		}
		blocks = append(blocks, "  "+name+"\n"+strings.TrimPrefix(vaultIndent(vaultSection("", rows, width-2)), "\n"))
	}
	var rest [][2]string
	for _, k := range slices.Sorted(maps.Keys(top)) {
		if k == "_comment" || (k == "agents" && vs != nil) {
			continue
		}
		rest = append(rest, vaultFlattenValue(k, top[k])...)
	}
	if len(rest) > 0 {
		blocks = append(blocks, strings.TrimPrefix(vaultSection("", rest, width), "\n"))
	}
	b.WriteString(strings.Join(blocks, "\n"))
	return b.String()
}

// vaultAgentActive is an agent's ACTIVE value: off only when its pin says off (claude has no pin, always on).
func vaultAgentActive(key string, pins map[string]string) string {
	if key != "" && strings.TrimSpace(pins[key]) == providers.AgentOff {
		return providers.AgentOff
	}
	return providers.AgentOn
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
