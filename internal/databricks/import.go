package databricks

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/databricks/dbxcfg"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// renameFlag collects repeatable --rename old=new pairs.
type renameFlag map[string]string

func (r renameFlag) String() string { return "" }

// Placeholder names the FLAGS placeholder for renameFlag (helptext.Placeholder).
func (r renameFlag) Placeholder() string { return "OLD=NEW" }

func (r renameFlag) Set(s string) error {
	old, id, ok := strings.Cut(s, "=")
	if !ok || old == "" || id == "" {
		return fmt.Errorf("--rename %q: want old=new", s)
	}
	r[old] = id
	return nil
}

var importPage = helptext.Page{
	Usage: "claude-code databricks import [flags]",
	About: "Create Databricks profiles from every section of a CLI config, read once. Dry-run unless --apply.",
}

// importCfg reads a databrickscfg once and creates vault provider rows; dry-run unless --apply.
func importCfg(args []string) int {
	fs := helptext.NewFlagSet("databricks import", importPage)
	file := fs.String("dbxcfg", "", "config path (default $DATABRICKS_CONFIG_FILE, else ~/.databrickscfg)")
	pick := fs.Bool("pick", false, "choose profiles in a picker (default: all)")
	apply := fs.Bool("apply", false, "write the profiles (default: print the diff)")
	seal := fs.Bool("seal-secrets", false, "seal client secrets and PATs in the vault")
	replace := fs.Bool("replace", false, "overwrite an existing row of the same id")
	rename := renameFlag{}
	fs.Var(rename, "rename", "import section OLD as profile NEW (repeatable)")
	if rest, err := outfmt.Parse(fs, args); err != nil {
		return helptext.ExitCode(err)
	} else if len(rest) > 0 {
		fs.Usage()
		return 2
	}
	path := *file
	if path == "" {
		path = dbxcfg.DefaultPath()
	}
	cands, err := dbxcfg.Candidates(path)
	if err != nil {
		return fail("import: %v", err)
	}
	if len(cands) == 0 {
		return fail("import: no profiles in %s", path)
	}
	if *pick {
		if cands, err = pickCandidates(cands); err != nil {
			return fail("import: %v", err)
		}
	}
	var rows []dbxcfg.Row
	oldDoc, newDoc, err := providers.UpdateOverlayDoc(false, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		rows = dbxcfg.Plan(cands, dbxcfg.Existing(top), dbxcfg.Options{Rename: rename, Replace: *replace})
		return dbxcfg.Apply(top, rows)
	})
	if err != nil {
		return fail("import: %v", err)
	}
	printImportTable(rows)
	if !printDiff(os.Stdout, oldDoc, newDoc) {
		fmt.Println("no changes")
	}
	if !slices.ContainsFunc(rows, func(r dbxcfg.Row) bool { return r.Action != "skip" }) {
		fmt.Println("nothing to import")
		return 0
	}
	if !*apply {
		fmt.Println("dry-run: pass --apply to write")
		return 0
	}
	if _, _, err := providers.UpdateOverlayDoc(true, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		return dbxcfg.Apply(top, rows)
	}); err != nil {
		return fail("import: %v", err)
	}
	fmt.Println("wrote profiles")
	if *seal {
		return sealImported(path, rows)
	}
	return 0
}

// sealImported stores each written row's client secret / PAT; the only place secrets are read.
func sealImported(path string, rows []dbxcfg.Row) int {
	code := 0
	for _, r := range rows {
		name := dbxcfg.SecretNameOf(r)
		if r.Action == "skip" || name == "" || !r.HasSecret {
			continue
		}
		s, err := dbxcfg.ReadSecrets(path, r.Name)
		val := s.ClientSecret
		if r.Auth == providers.AuthPAT {
			val = s.Token
		}
		if err == nil && val != "" {
			err = secrets.Store(name, val)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "claude-code: seal %s: %v\n", name, err)
			code = 1
			continue
		}
		fmt.Printf("sealed %s\n", name)
	}
	return code
}

func printImportTable(rows []dbxcfg.Row) {
	fmt.Printf("%-32s %-10s %-46s %-6s %-7s %s\n", "PROFILE", "AUTHTYPE", "HOST", "SECRET", "ACTION", "NOTE")
	for _, r := range rows {
		kind, has := r.Auth, "no"
		if kind == "" {
			kind = "-"
		}
		if r.HasSecret {
			has = "yes"
		}
		id := r.Name
		if r.ID != r.Name {
			id += "->" + r.ID
		}
		fmt.Printf("%-32s %-10s %-46s %-6s %-7s %s\n", id, kind, r.Host, has, r.Action, r.Reason)
	}
}

// pickCandidates toggles candidates in the arrow-key picker until Done; Esc cancels.
func pickCandidates(cands []dbxcfg.Candidate) ([]dbxcfg.Candidate, error) {
	labels := make([][2]string, len(cands))
	for i, c := range cands {
		labels[i] = [2]string{c.Name, c.Host}
	}
	on, err := pickToggle("Import profiles (Enter toggles)", "import the checked profiles", labels)
	if err != nil {
		return nil, err
	}
	var out []dbxcfg.Candidate
	for i, c := range cands {
		if on[i] {
			out = append(out, c)
		}
	}
	return out, nil
}

// pickToggle is a checklist in the arrow-key picker (all checked at start); Done returns the marks, Esc cancels.
func pickToggle(title, doneNote string, labels [][2]string) ([]bool, error) {
	on := make([]bool, len(labels))
	for i := range on {
		on[i] = true
	}
	at := 0
	for {
		rows := make([][2]string, 0, len(labels)+1)
		for i, l := range labels {
			mark := "[ ]"
			if on[i] {
				mark = "[x]"
			}
			rows = append(rows, [2]string{mark + " " + l[0], l[1]})
		}
		rows = append(rows, [2]string{"Done", doneNote})
		i, err := tui.Pick(title, rows, at)
		if err != nil {
			return nil, err
		}
		switch {
		case i < 0:
			return nil, fmt.Errorf("cancelled")
		case i == len(labels):
			return on, nil
		}
		on[i], at = !on[i], i
	}
}
