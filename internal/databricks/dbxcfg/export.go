package dbxcfg

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

// ExportRow is one vault row rendered as a .databrickscfg section; it carries identifiers only, never a secret.
type ExportRow struct {
	ID          string // vault provider id
	Section     string // section name (ID unless renamed)
	Host        string
	Auth        string // row auth.type: oauth | m2m | pat
	ClientID    string
	AccountID   string
	WorkspaceID string
}

// ExportRows lists the registry's databricks preset rows with an authType; skipped explains the others.
func ExportRows(reg *providers.Registry) (rows []ExportRow, skipped []string) {
	names := make([]string, 0, len(reg.Providers))
	for n := range reg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
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
		rows = append(rows, ExportRow{ID: n, Section: n, Host: host, Auth: p.Auth.Type, ClientID: p.Auth.ClientID,
			AccountID: p.Auth.AccountID, WorkspaceID: p.Auth.WorkspaceID})
	}
	return rows, skipped
}

var sectionRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Rename applies id=section renames and drops rows whose section is reserved ([DEFAULT], __settings__) or malformed.
func Rename(rows []ExportRow, rename map[string]string) (out []ExportRow, skipped []string) {
	seen := map[string]bool{}
	for _, r := range rows {
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
	return out, skipped
}

// sectionText renders one row; u2m is written as the CLI's databricks-cli, and pat names itself so it re-imports.
func sectionText(r ExportRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\nhost = %s\n", r.Section, r.Host)
	switch r.Auth {
	case providers.AuthOAuth:
		b.WriteString("auth_type = " + CLIAuthU2M + "\n")
	case providers.AuthM2M:
		fmt.Fprintf(&b, "client_id = %s\nauth_type = %s\n", r.ClientID, CLIAuthM2M)
	case providers.AuthPAT:
		b.WriteString("auth_type = " + CLIAuthPAT + "\n")
	}
	if r.AccountID != "" {
		fmt.Fprintf(&b, "account_id = %s\n", r.AccountID)
	}
	if r.WorkspaceID != "" {
		fmt.Fprintf(&b, "workspace_id = %s\n", r.WorkspaceID)
	}
	return b.String()
}

// Render is a fresh config holding rows, one section each.
func Render(rows []ExportRow) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, sectionText(r))
	}
	return strings.Join(parts, "\n")
}

// Hint is the use-time line for a row: how the CLI gets the credential the file deliberately lacks.
func Hint(r ExportRow) string {
	switch r.Auth {
	case providers.AuthM2M:
		return fmt.Sprintf("[%s] DATABRICKS_CLIENT_SECRET=$(claude-code vault get %s) databricks -p %s ...",
			r.Section, secretName(r.ID, r.Auth), r.Section)
	case providers.AuthPAT:
		return fmt.Sprintf("[%s] DATABRICKS_TOKEN=$(claude-code vault get %s) databricks -p %s ...",
			r.Section, secretName(r.ID, r.Auth), r.Section)
	}
	return fmt.Sprintf("[%s] databricks auth login -p %s (the CLI keeps its own login)", r.Section, r.Section)
}

// block is one raw run of lines: the preamble (name "") or a [header] and its body.
type block struct {
	name  string
	lines []string
}

func splitBlocks(text string) []block {
	blocks := []block{{}}
	for _, l := range strings.SplitAfter(text, "\n") {
		if l == "" {
			continue
		}
		t := strings.TrimSpace(strings.TrimPrefix(l, "\ufeff"))
		if strings.HasPrefix(t, "[") {
			if name, ok := headerName(t); ok {
				blocks = append(blocks, block{name: name})
			}
		}
		last := &blocks[len(blocks)-1]
		last.lines = append(last.lines, l)
	}
	return blocks
}

// Merge writes rows into existing text: every other section (and [DEFAULT]) stays verbatim. An existing
// section of the same name is replaced only with force, and an auth_type = databricks-cli one never is.
func Merge(existing string, rows []ExportRow, force bool) (string, []string) {
	have := map[string]map[string]string{}
	for _, s := range parseSections(existing) {
		have[s.name] = s.keys
	}
	blocks := splitBlocks(existing)
	var notes []string
	var appended []string
	for _, r := range rows {
		keys, exists := have[r.Section]
		switch {
		case isReserved(r.Section):
			notes = append(notes, "keep ["+r.Section+"]: reserved, never written")
		case !exists:
			appended = append(appended, sectionText(r))
			notes = append(notes, "add ["+r.Section+"]")
		case keys["auth_type"] == CLIAuthU2M:
			notes = append(notes, "keep ["+r.Section+"]: an existing databricks-cli login section is never overwritten")
		case !force:
			notes = append(notes, "keep ["+r.Section+"]: exists (--force replaces it)")
		default:
			blocks = replaceBlocks(blocks, r.Section, sectionText(r))
			notes = append(notes, "replace ["+r.Section+"]")
		}
	}
	var b strings.Builder
	for _, bl := range blocks {
		b.WriteString(strings.Join(bl.lines, ""))
	}
	out := b.String()
	if len(appended) > 0 {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		if out != "" && !strings.HasSuffix(out, "\n\n") {
			out += "\n"
		}
		out += strings.Join(appended, "\n")
	}
	return out, notes
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
