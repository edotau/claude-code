package databricks

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var exportPage = helptext.Page{
	Usage: "claude-code databricks export [flags]",
	About: "Write the vault's databricks rows as .databrickscfg sections. No secret is\n" +
		"ever written: each row's use-time hint goes to stderr. Prints to stdout\n" +
		"unless --out; --merge edits an existing file in place.",
}

// export renders vault rows as .databrickscfg sections: stdout, a new --out file, or --merge into one.
func export(args []string) int {
	fs := helptext.NewFlagSet("databricks export", exportPage)
	pick := fs.Bool("pick", false, "choose rows in a picker (default: all)")
	out := fs.String("out", "", "write to FILE (0600) instead of stdout")
	merge := fs.Bool("merge", false, "merge into --out (default the CLI's config), others kept")
	force := fs.Bool("force", false, "overwrite --out, or with --merge replace same-name sections (never a databricks-cli login)")
	rename := renameFlag{}
	fs.Var(rename, "rename", "write row OLD as section NEW (repeatable)")
	if rest, err := helptext.ParseArgs(fs, args); err != nil {
		return helptext.ExitCode(err)
	} else if len(rest) > 0 {
		fs.Usage()
		return 2
	}
	rows, code := exportRowsFor(*pick, rename)
	if code != 0 {
		return code
	}
	written, code := writeExport(rows, *out, *merge, *force)
	if code != 0 {
		return code
	}
	for _, r := range written {
		fmt.Fprintln(os.Stderr, "use: "+rowHint(r))
	}
	return 0
}

// exportRowsFor loads the vault rows, applies --pick and --rename, reports skips, and returns the rows to write.
func exportRowsFor(pick bool, rename renameFlag) ([]exportRow, int) {
	reg, err := providers.Load()
	if err != nil {
		return nil, fail("export: %v", err)
	}
	rows, skipped := exportRows(reg)
	if pick && len(rows) > 0 {
		if rows, err = pickExport(rows); err != nil {
			return nil, fail("export: %v", err)
		}
	}
	rows, more := renameRows(rows, rename)
	for _, s := range append(skipped, more...) {
		fmt.Fprintln(os.Stderr, "skip "+s)
	}
	if len(rows) == 0 {
		return nil, fail("export: no databricks rows with an authType to export")
	}
	return rows, 0
}

// writeExport writes rows and returns those actually written (a merge keeps existing sections).
func writeExport(rows []exportRow, out string, merge, force bool) ([]exportRow, int) {
	if !merge {
		text := renderExport(rows)
		if out == "" {
			if _, err := io.WriteString(os.Stdout, text); err != nil {
				return nil, fail("export: %v", err)
			}
			return rows, 0
		}
		if err := writeFile0600(out, text, force); err != nil {
			return nil, fail("export: %v", err)
		}
		fmt.Fprintf(os.Stderr, "wrote %d sections to %s\n", len(rows), out)
		return rows, 0
	}
	if out == "" {
		var err error
		if out, err = defaultPath(); err != nil {
			return nil, fail("export: %v", err)
		}
	}
	out, err := paths.ResolveLink(out, true)
	if err != nil {
		return nil, fail("export: %v", err)
	}
	var msg string
	var written []exportRow
	if err := paths.WithFileLock(out, func() error {
		var err error
		msg, written, err = mergeInto(out, rows, force)
		return err
	}); err != nil {
		return nil, fail("export: %v", err)
	}
	fmt.Fprint(os.Stderr, msg)
	return written, 0
}

// mergeInto merges rows into the config at out (the caller holds its lock); it returns the stderr summary and the rows written.
func mergeInto(out string, rows []exportRow, force bool) (string, []exportRow, error) {
	old, err := os.ReadFile(out)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	if _, _, err := parseSections(string(old)); err != nil {
		return "", nil, fmt.Errorf("%s: %w", out, err)
	}
	text, notes, written := mergeExport(string(old), rows, force)
	msg := strings.Join(notes, "\n")
	if len(notes) > 0 {
		msg += "\n"
	}
	if text == string(old) {
		return msg + out + " unchanged\n", written, nil
	}
	if err := paths.AtomicWrite(out, []byte(text), 0o600); err != nil {
		return "", nil, err
	}
	return msg + "merged into " + out + "\n", written, nil
}

func pickExport(rows []exportRow) ([]exportRow, error) {
	return pickSubset("Export rows (Enter toggles)", "export the checked rows", rows,
		func(r exportRow) [2]string { return [2]string{r.ID, r.Auth + " " + r.Host} })
}

// writeFile0600 writes text to path atomically with mode 0600; an existing path needs force.
func writeFile0600(path, text string, force bool) error {
	path, err := paths.ResolveLink(path, true)
	if err != nil {
		return err
	}
	return paths.WithFileLock(path, func() error {
		_, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && !force {
			return fmt.Errorf("%s exists; pass --force to overwrite", path)
		}
		return paths.AtomicWrite(path, []byte(text), 0o600)
	})
}

// exportRow is one vault row rendered as a .databrickscfg section; it carries identifiers only, never a secret.
type exportRow struct {
	Pair        string // user section named by an SP pairing signal
	PairSet     bool
	SecretID    string // paired user row owns this SP credential
	ID          string // vault provider id
	Section     string // section name (ID unless renamed)
	Host        string
	Auth        string // row auth.type: oauth | m2m | pat
	ClientID    string
	AccountID   string
	WorkspaceID string
}

// exportRows lists the registry's databricks preset rows with an authType; skipped explains the others.
func exportRows(reg *providers.Registry) (rows []exportRow, skipped []string) {
	rows, skipped = presetRows(reg)
	markM2MPairs(rows)
	return appendSPRows(reg, rows), skipped
}

// presetRows is one row per exportable databricks preset provider, sorted by name.
func presetRows(reg *providers.Registry) (rows []exportRow, skipped []string) {
	for _, n := range slices.Sorted(maps.Keys(reg.Providers)) {
		p := reg.Providers[n]
		if p == nil || p.Preset != providers.PresetDatabricks {
			continue
		}
		if m := p.Auth.Method(); m != providers.AuthOAuth && m != providers.AuthM2M && m != providers.AuthPAT {
			skipped = append(skipped, n+": no oauth/m2m/pat auth (delegates to the CLI's own profile); not exported")
			continue
		}
		host := providers.AuthHost(p)
		if host == "" {
			skipped = append(skipped, n+": no https host; not exported")
			continue
		}
		rows = append(rows, exportRow{ID: n, Section: n, Host: host, Auth: p.Auth.Type, ClientID: p.Auth.ClientID,
			AccountID: p.Auth.AccountID, WorkspaceID: p.Auth.WorkspaceID})
	}
	return rows, skipped
}

// markM2MPairs records a no-pairing decision for each m2m row that shares a host with an oauth row.
func markM2MPairs(rows []exportRow) {
	for i, r := range rows {
		if r.Auth != providers.AuthM2M {
			continue
		}
		for _, u := range rows {
			if u.Auth == providers.AuthOAuth && u.Host == r.Host {
				rows[i].PairSet = true
				break
			}
		}
	}
}

// appendSPRows adds a paired m2m row for every oauth row that carries an spClientId.
func appendSPRows(reg *providers.Registry, rows []exportRow) []exportRow {
	for _, r := range rows {
		p := reg.Providers[r.ID]
		if r.Auth != providers.AuthOAuth || p.Auth.SPClientID == "" {
			continue
		}
		section := freeSPSection(reg, rows, r.ID)
		rows = append(rows, exportRow{ID: section, Section: section, Host: r.Host, Auth: providers.AuthM2M,
			ClientID: p.Auth.SPClientID, Pair: r.Section, PairSet: true, SecretID: r.ID})
	}
	return rows
}

// freeSPSection is the first of id-sp, id-sp-2, ... that no provider or row already uses.
func freeSPSection(reg *providers.Registry, rows []exportRow, id string) string {
	section := id + "-sp"
	for i := 2; ; i++ {
		if _, exists := reg.Providers[section]; !exists && !slices.ContainsFunc(rows, func(x exportRow) bool { return x.Section == section }) {
			return section
		}
		section = fmt.Sprintf("%s-sp-%d", id, i)
	}
}

var sectionRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// renameRows applies id=section renames and drops rows whose section is reserved, malformed or taken.
func renameRows(rows []exportRow, rename map[string]string) (out []exportRow, skipped []string) {
	seen, used := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		used[r.ID], used[r.Pair] = true, true
		if s, ok := rename[r.Pair]; ok && r.PairSet {
			r.Pair = s
		}
		if s, ok := rename[r.ID]; ok {
			r.Section = s
		}
		switch {
		case isReserved(r.Section):
			skipped = append(skipped, r.ID+": section ["+r.Section+"] is reserved; never written")
		case !sectionRE.MatchString(r.Section):
			skipped = append(skipped, r.ID+": section name "+r.Section+" must match [A-Za-z0-9_.-]+")
		case seen[r.Section]:
			skipped = append(skipped, r.ID+": section ["+r.Section+"] is already taken by another row")
		default:
			seen[r.Section] = true
			out = append(out, r)
		}
	}
	for _, old := range slices.Sorted(maps.Keys(rename)) {
		if !used[old] {
			skipped = append(skipped, "--rename "+old+": matches no exported row")
		}
	}
	return out, skipped
}

// sectionText renders one row; u2m is written as the CLI's databricks-cli, and pat names itself so it re-imports.
func sectionText(r exportRow) string {
	var b strings.Builder
	if r.PairSet {
		pair := r.Pair
		if pair == "" {
			pair = "none"
		}
		fmt.Fprintf(&b, "# claude-code: pair=%s\n", pair)
	}
	fmt.Fprintf(&b, "[%s]\nhost = %s\n", r.Section, r.Host)
	switch r.Auth {
	case providers.AuthOAuth:
		b.WriteString("auth_type = " + cliAuthU2M + "\n")
	case providers.AuthM2M:
		fmt.Fprintf(&b, "client_id = %s\nauth_type = %s\n", r.ClientID, cliAuthM2M)
	case providers.AuthPAT:
		b.WriteString("auth_type = " + cliAuthPAT + "\n")
	}
	if r.AccountID != "" {
		fmt.Fprintf(&b, "account_id = %s\n", r.AccountID)
	}
	if r.WorkspaceID != "" {
		fmt.Fprintf(&b, "workspace_id = %s\n", r.WorkspaceID)
	}
	return b.String()
}

// renderExport is a fresh config holding rows, one section each.
func renderExport(rows []exportRow) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, sectionText(r))
	}
	return strings.Join(parts, "\n")
}

// rowHintEnv is the env var the CLI reads each secret-backed auth kind's credential from.
var rowHintEnv = map[string]string{providers.AuthM2M: "DATABRICKS_CLIENT_SECRET", providers.AuthPAT: "DATABRICKS_TOKEN"}

// rowHint is the use-time line for a row: how the CLI gets the credential the file deliberately lacks.
func rowHint(r exportRow) string {
	if r.SecretID != "" {
		r.ID = r.SecretID
	}
	if env, ok := rowHintEnv[r.Auth]; ok {
		return fmt.Sprintf("[%s] %s=$(claude-code vault get %s) databricks -p %s ...",
			r.Section, env, secretName(r.ID, r.Auth), r.Section)
	}
	return fmt.Sprintf("[%s] databricks auth login -p %s (the CLI keeps its own login)", r.Section, r.Section)
}

// block is one raw run of lines: the preamble (name "") or a [header] and its body.
type block struct {
	name  string
	lines []string
}

// detachHeaderPrefix removes the next header's pairing comment and blank lines from prev and returns them.
func detachHeaderPrefix(prev *block) []string {
	start := len(prev.lines)
	for start > 0 && strings.TrimSpace(prev.lines[start-1]) == "" {
		start--
	}
	if start > 0 {
		if _, ok := pairingComment(prev.lines[start-1]); ok {
			start--
		} else {
			start = len(prev.lines)
		}
	}
	prefix := slices.Clone(prev.lines[start:])
	prev.lines = prev.lines[:start]
	return prefix
}

func splitBlocks(text string) []block {
	blocks := []block{{}}
	for _, l := range strings.SplitAfter(text, "\n") {
		if l == "" {
			continue
		}
		t := strings.TrimSpace(strings.TrimPrefix(l, "\ufeff"))
		if name, ok := headerName(t); ok {
			prefix := detachHeaderPrefix(&blocks[len(blocks)-1])
			blocks = append(blocks, block{name: name, lines: prefix})
		}
		last := &blocks[len(blocks)-1]
		last.lines = append(last.lines, l)
	}
	return blocks
}

// mergeExport writes rows into existing text; a same-name section needs force and a databricks-cli one never is.
func mergeExport(existing string, rows []exportRow, force bool) (string, []string, []exportRow) {
	have := map[string]*section{}
	secs, _, _ := parseSections(existing) // mergeInto has already refused a file that does not parse
	for _, s := range secs {
		have[s.name] = s
	}
	m := &merger{have: have, blocks: splitBlocks(existing), force: force}
	for _, r := range rows {
		m.apply(r)
	}
	var b strings.Builder
	for _, bl := range m.blocks {
		b.WriteString(strings.Join(bl.lines, ""))
	}
	out := b.String()
	if len(m.appended) > 0 {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		if out != "" && !strings.HasSuffix(out, "\n\n") {
			out += "\n"
		}
		out += strings.Join(m.appended, "\n")
	}
	return out, m.notes, m.written
}

// merger accumulates mergeExport's edits: the rewritten blocks, the sections to append and the per-row notes.
type merger struct {
	have     map[string]*section
	blocks   []block
	notes    []string
	appended []string
	written  []exportRow
	force    bool
}

// apply merges one row: keep reserved and databricks-cli sections, add new ones, replace only with force.
func (m *merger) apply(r exportRow) {
	sig, exists := m.have[r.Section]
	if exists && sig.pairSet {
		r.Pair, r.PairSet = sig.pair, true
	}
	switch {
	case isReserved(r.Section):
		m.notes = append(m.notes, "keep ["+r.Section+"]: reserved, never written")
	case !exists:
		m.appended = append(m.appended, sectionText(r))
		m.written = append(m.written, r)
		m.notes = append(m.notes, "add ["+r.Section+"]")
	case sig.keys["auth_type"] == cliAuthU2M:
		m.notes = append(m.notes, "keep ["+r.Section+"]: an existing databricks-cli login section is never overwritten")
	case !m.force:
		m.notes = append(m.notes, "keep ["+r.Section+"]: exists (--force replaces it)")
	default:
		m.blocks = replaceBlocks(m.blocks, r.Section, sectionText(r))
		m.written = append(m.written, r)
		m.notes = append(m.notes, "replace ["+r.Section+"]")
	}
}

// replaceBlocks puts text where the first [name] block was and drops its duplicates.
func replaceBlocks(blocks []block, name, text string) []block {
	var out []block
	done := false
	for _, bl := range blocks {
		if bl.name != name {
			out = append(out, bl)
			continue
		}
		if !done {
			lines := []string{text}
			if n := len(bl.lines); n > 0 && strings.TrimSpace(bl.lines[n-1]) == "" {
				lines = append(lines, "\n") // keep the blank separator before the next section
			}
			out = append(out, block{name: name, lines: lines})
			done = true
		}
	}
	return out
}
