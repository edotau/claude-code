// Package vaultdoc is the vault's non-secret document: `vault ls` sections, tables and JSON, and the edit round trip.
package vaultdoc

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

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/table"
	"github.com/edotau/claude-code/internal/vault"
)

// Width is $COLUMNS when set, else the stdout terminal's width, else 80 when stdout is not a terminal.
func Width() int {
	if n := ansi.Columns(0); n > 0 {
		return n
	}
	if n, _, err := proc.Size(int(os.Stdout.Fd())); err == nil && n > 0 {
		return n
	}
	return 80
}

// Text lays out each selected section (uppercase heading + aligned rows, in
// Sections order), painting every section when color is on (agents paints its own rows).
func Text(doc Doc, stored []vault.Stored, sel []string, width int, color bool) string {
	render := map[string]func() string{
		"secrets":   func() string { return vaultSecretsText(stored, width) },
		"pins":      func() string { return vaultSection("PINS", vaultMapRows(doc.Pins), width) },
		"env":       func() string { return vaultSection("ENV", vaultMapRows(doc.Env), width) },
		"providers": func() string { return vaultProvidersText(doc.Providers, width) },
		"routing":   func() string { return vaultRoutingText(doc.Providers, width) },
		"agents":    func() string { return vaultAgentsText(doc.Agents, width, color) },
		"settings":  func() string { return vaultSection("SETTINGS", vaultFlattenSection(doc.Settings), width) },
		"vault":     func() string { return vaultSection("VAULT", vaultVaultRows(doc.Vault.KeyCommand), width) },
	}
	var b strings.Builder
	for _, s := range sel {
		out := render[s]()
		if len(sel) == len(Sections) && emptySection(out) { // bare/all hides empty sections; named ones say (none)
			continue
		}
		if color && s != "agents" {
			out = paintMarks(helptext.Paint(out))
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

// isBlankJSON reports an absent, empty or null overlay section.
func isBlankJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// vaultFlattenSection flattens an overlay to dotted-path rows.
func vaultFlattenSection(raw json.RawMessage) [][2]string {
	if isBlankJSON(raw) {
		return nil
	}
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return [][2]string{{"error", err.Error()}}
	}
	return vaultFlattenValue("", top)
}

// vaultFlattenValue flattens one decoded value under prefix to sorted-key path/value rows.
func vaultFlattenValue(prefix string, v any) [][2]string {
	var rows [][2]string
	switch t := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			rows = append(rows, vaultFlattenValue(p, t[k])...)
		}
	case []any:
		for i, vv := range t {
			rows = append(rows, vaultFlattenValue(fmt.Sprintf("%s[%d]", prefix, i), vv)...)
		}
	case nil:
		rows = append(rows, [2]string{prefix, "null"})
	case string:
		rows = append(rows, [2]string{prefix, t})
	case bool:
		rows = append(rows, [2]string{prefix, strconv.FormatBool(t)})
	case float64:
		rows = append(rows, [2]string{prefix, strconv.FormatFloat(t, 'g', -1, 64)})
	default:
		rows = append(rows, [2]string{prefix, fmt.Sprint(t)})
	}
	return rows
}

// Key/value sections (PINS, ENV, ROUTING, SETTINGS, AGENTS, VAULT) clip a longer key or value with "…".
const (
	kvKeyMax   = 24
	kvValueMax = 48
)

// vaultSection renders one heading plus its aligned, width-truncated rows (or "(none)" when empty).
func vaultSection(heading string, rows [][2]string, width int) string {
	rows = slices.Clone(rows)
	for i, r := range rows {
		r[0] = table.TruncateMiddle(r[0], kvKeyMax) // keys differ at the end ("…Markers[0]" vs "…Markers[1]")
		r[1] = table.Truncate(r[1], kvValueMax)
		rows[i] = r
	}
	rows = clipValues(rows, width)
	var b strings.Builder
	b.WriteString(heading)
	b.WriteByte('\n')
	if len(rows) == 0 {
		b.WriteString("  (none)\n")
		return b.String()
	}
	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = []string{r[0], r[1]}
	}
	b.WriteString(table.Indented(cells, width, nil))
	return b.String()
}

// clipValues clips values to the room left after the widest key, so a narrow terminal clips the value, not the key.
func clipValues(rows [][2]string, width int) [][2]string {
	keyW := 0
	for _, r := range rows {
		keyW = max(keyW, utf8.RuneCountInString(r[0]))
	}
	room := width - 2 - keyW - 2 // indent and the minimum column gap
	if width <= 0 || room < 1 {
		return rows
	}
	for i, r := range rows {
		rows[i][1] = table.Truncate(r[1], room)
	}
	return rows
}

// vaultProvidersText renders the providers overlay as a provider table (PLANS stays JSON-only) and one-line lists.
func vaultProvidersText(raw json.RawMessage, width int) string {
	var b strings.Builder
	b.WriteString("PROVIDERS\n")
	if isBlankJSON(raw) {
		b.WriteString("  (none)\n")
		return b.String()
	}
	var reg providers.Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		b.WriteString(table.Truncate("  error  "+err.Error(), width))
		b.WriteByte('\n')
		return b.String()
	}
	var top struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	_ = json.Unmarshal(raw, &top)
	nested := top.Providers
	var blocks []string
	if rows := vaultProviderRows(nested); len(rows) > 0 {
		head := []string{"NAME", "PROVIDER", "AUTH", "OPUS", "SONNET", "HAIKU", "OPENCODE", "FALLBACK"}
		head, cols, rows := table.DropEmptyCols(head, providerColMax, rows)
		blocks = append(blocks, table.Render(head, cols, rows, width))
	}
	if lines := vaultProviderLines(reg); len(lines) > 0 {
		blocks = append(blocks, strings.TrimPrefix(vaultSection("", lines, width), "\n"))
	}
	if len(blocks) == 0 {
		b.WriteString("  (none)\n")
	}
	b.WriteString(strings.Join(blocks, "\n"))
	return b.String()
}

// vaultProviderRows is the overlay's provider table rows: profiles, then presets as one row each, shown rows only.
func vaultProviderRows(nested map[string]json.RawMessage) [][]string {
	var rows [][]string
	type presetKey struct{ prov, preset string }
	presets := map[presetKey][]string{} // provider+preset → its env rows, shown as one row
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
			switch {
			case p == nil: // "name": null removes a built-in
				rows = append(rows, []string{name, prov, "(removed)", "", "", "", "", ""})
			case p.Preset != "":
				k := presetKey{prov, p.Preset}
				presets[k] = append(presets[k], name)
			default:
				rows = append(rows, []string{name, prov, p.Auth.Method(), p.Models["opus"], p.Models["sonnet"],
					p.Models["haiku"], p.OpencodeID, p.OpencodeFallback})
			}
		}
	}
	keys := slices.SortedFunc(maps.Keys(presets), func(a, b presetKey) int {
		return cmp.Or(cmp.Compare(a.preset, b.preset), cmp.Compare(a.prov, b.prov))
	})
	for _, k := range keys {
		rows = append(rows, []string{k.preset, k.prov, presetEnvs(presets[k]), "", "", "", "", ""})
	}
	return vaultShownRows(rows)
}

// vaultProviderLines is the overlay's one-line settings (default, fallback, plugins, plan format) that are set.
func vaultProviderLines(reg providers.Registry) [][2]string {
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
	return lines
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

// credOK reports whether provider name's credential resolves in the merged registry (a seam for tests).
var credOK = func(name string) bool {
	reg, err := providers.Load()
	if err != nil {
		return false
	}
	p, ok := reg.Providers[name]
	return ok && providers.CredentialPresent(p)
}

// presetEnvs lists a preset's env rows by name, each marked ✓ (credential resolves) or ✗.
func presetEnvs(names []string) string {
	ok := map[string]bool{}
	for _, n := range names {
		ok[n] = credOK(n)
	}
	slices.SortStableFunc(names, func(a, b string) int { // failures first so a clipped cell still shows them
		if ok[a] != ok[b] {
			if ok[a] {
				return 1
			}
			return -1
		}
		return cmp.Compare(a, b)
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

// vaultRoutingText renders the providers overlay's nested routing object (weights, cells, subagents,
// opencode's sub-pick knobs) as dotted-path rows, in the style of vaultProvidersText's own flatten.
func vaultRoutingText(raw json.RawMessage, width int) string {
	var b strings.Builder
	b.WriteString("ROUTING\n")
	if isBlankJSON(raw) {
		b.WriteString("  (none)\n")
		return b.String()
	}
	var reg providers.Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		b.WriteString(table.Truncate("  error  "+err.Error(), width))
		b.WriteByte('\n')
		return b.String()
	}
	rb, err := json.Marshal(reg.Routing)
	if err != nil {
		b.WriteString(table.Truncate("  error  "+err.Error(), width))
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

// vaultRoutingJSON is `vault ls --format json`'s "routing" value: the providers overlay's nested
// routing object, or null when the overlay carries none.
func vaultRoutingJSON(providersRaw json.RawMessage) (json.RawMessage, error) {
	if isBlankJSON(providersRaw) {
		return json.RawMessage("null"), nil
	}
	var reg providers.Registry
	if err := json.Unmarshal(providersRaw, &reg); err != nil {
		return nil, err
	}
	return json.Marshal(reg.Routing)
}

// Per-column max widths for the PROVIDERS table; a longer cell is clipped with "…" (0 = no cap).
var providerColMax = []int{20, 10, 20, 14, 14, 16, 16, 18}

// vaultAgentCols are the leading agent-table columns; other pin suffixes follow alphabetically.
var vaultAgentCols = []string{"PROFILE", "MODEL", "EFFORT", "CONTEXT"}

// vaultOverlayAgents decodes the agents overlay: the top-level object and its "agents" map.
func vaultOverlayAgents(raw json.RawMessage) (top, vs map[string]any, err error) {
	if !isBlankJSON(raw) {
		if err = json.Unmarshal(raw, &top); err != nil {
			return nil, nil, err
		}
	}
	vs, _ = top["agents"].(map[string]any)
	return top, vs, nil
}

// vaultRecordCells is one agent record as table cells; claude's aliases fold into MODEL as alias=id.
func vaultRecordCells(name string, r vault.AgentRecord) map[string]string {
	cells := map[string]string{"PROFILE": r.Profile, "MODEL": r.Model, "EFFORT": r.Effort, "CONTEXT": r.Context, "ACCOUNT": r.Account}
	for _, alias := range models.AliasNames {
		if v := r.Aliases[alias]; v != "" {
			cells["MODEL"] = strings.TrimSpace(cells["MODEL"] + " " + alias + "=" + v)
		}
	}
	for k, v := range cells {
		if strings.TrimSpace(v) == "" {
			delete(cells, k)
		}
	}
	return cells
}

// vaultAgentTable is the AGENT/PROFILE/MODEL/... table, one row per agent; empty columns drop.
func vaultAgentTable(names []string, width int, color bool) string {
	recs := vault.AgentRecords(agent.RosterNames())
	per, cols := vaultAgentCells(names, recs)
	head, colMax := append([]string{"AGENT"}, cols...), []int{0}
	for _, c := range cols {
		colMax = append(colMax, map[string]int{"MODEL": 60, "PROFILE": 24}[c])
	}
	rows, codes := make([][]string, len(names)), make([]string, len(names))
	for i, n := range names {
		rows[i], codes[i] = vaultAgentRow(n, cols, per[i], recs[n].Profile == "", color)
	}
	head, colMax, rows = table.DropEmptyCols(head, colMax, rows)
	return table.Styled(head, colMax, rows, width, codes, color)
}

// vaultAgentCells is each agent's record cells plus the columns: the fixed ones, then any extra sorted.
func vaultAgentCells(names []string, recs map[string]vault.AgentRecord) ([]map[string]string, []string) {
	per := make([]map[string]string, len(names))
	extra := map[string]bool{}
	for i, n := range names {
		per[i] = vaultRecordCells(n, recs[n])
		for k := range per[i] {
			if !slices.Contains(vaultAgentCols, k) {
				extra[k] = true
			}
		}
	}
	return per, append(slices.Clone(vaultAgentCols), slices.Sorted(maps.Keys(extra))...)
}

// vaultAgentRow is agent n's row and color: cyan when on, dim when off with no record, " (off)" without color.
func vaultAgentRow(n string, cols []string, cells map[string]string, noProfile, color bool) ([]string, string) {
	row := []string{n}
	for _, c := range cols {
		row = append(row, cells[c])
	}
	off := !agent.VaultAgentIsClaude(n) && noProfile
	code := ""
	switch {
	case !off:
		code = ansi.Cyan
	case len(cells) == 0:
		code = ansi.Dim
	}
	if off && !color {
		row[0] += " (off)"
	}
	return row, code
}

// vaultAgentsText renders the agent table, then the per-agent overlay rows (and other top-level keys) below it.
func vaultAgentsText(raw json.RawMessage, width int, color bool) string {
	top, vs, err := vaultOverlayAgents(raw)
	if err != nil {
		return vaultSection("AGENTS", vaultFlattenSection(raw), width)
	}
	names := agent.VaultAgentNames(vs)
	rows := append(vaultAgentOverlayRows(names, vs), vaultTopLevelRows(top, vs)...)
	out := vaultAgentTable(names, width, color)
	if len(rows) == 0 {
		return out
	}
	return out + "\n" + vaultOverlayLines(rows, width)
}

// vaultAgentOverlayRows is each named agent's overlay entry as agent/path/value rows.
func vaultAgentOverlayRows(names []string, vs map[string]any) [][3]string {
	var rows [][3]string
	for _, name := range names {
		if v, ok := vs[name]; ok {
			for _, r := range vaultAgentRows(v) {
				rows = append(rows, [3]string{name, r[0], r[1]})
			}
		}
	}
	return rows
}

// vaultTopLevelRows is the overlay's other top-level keys as agentless rows; _comment, records and agents skip.
func vaultTopLevelRows(top, vs map[string]any) [][3]string {
	var rows [][3]string
	for _, k := range slices.Sorted(maps.Keys(top)) {
		if k == "_comment" || k == "records" || (k == "agents" && vs != nil) {
			continue
		}
		for _, r := range vaultFlattenValue(k, top[k]) {
			rows = append(rows, [3]string{"", r[0], r[1]})
		}
	}
	return rows
}

// vaultOverlayLines aligns agent/path/value rows (path and value clipped like the key/value sections).
func vaultOverlayLines(rows [][3]string, width int) string {
	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = []string{r[0], table.TruncateMiddle(r[1], kvKeyMax+8), table.Truncate(r[2], kvValueMax)}
	}
	return table.Indented(cells, width, nil)
}

// vaultAgentRows renders one agent entry: launch.env, launch.args, permission and mcp.allow read as lists.
func vaultAgentRows(v any) [][2]string {
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
			rows = append(rows, vaultPermissionRow(m[k]))
		case (k == "launch" || k == "mcp") && isMap:
			rows = append(rows, vaultListRows(k, sub)...)
		default:
			rows = append(rows, vaultFlattenValue(k, m[k])...)
		}
	}
	return rows
}

// vaultPermissionRow folds an agent's permission object into one "permission  k=v, ..." row.
func vaultPermissionRow(v any) [2]string {
	var parts []string
	for _, r := range vaultFlattenValue("", v) {
		parts = append(parts, r[0]+"="+r[1])
	}
	return [2]string{"permission", strings.Join(parts, ", ")}
}

// vaultListRows renders launch or mcp sub-keys; launch.env, launch.args and mcp.allow string lists join on one row.
func vaultListRows(k string, sub map[string]any) [][2]string {
	var rows [][2]string
	for _, sk := range slices.Sorted(maps.Keys(sub)) {
		path, sep := k+"."+sk, map[string]string{"launch.env": ", ", "launch.args": " ", "mcp.allow": ", "}[k+"."+sk]
		if s, ok := vaultStringList(sub[sk]); ok && sep != "" {
			rows = append(rows, [2]string{path, strings.Join(s, sep)})
			continue
		}
		rows = append(rows, vaultFlattenValue(path, sub[sk])...)
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
