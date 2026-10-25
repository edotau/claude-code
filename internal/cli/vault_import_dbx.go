package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/dbximport"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/profile"
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

// wantsDBXImport reports a `vault import --from databrickscfg` invocation.
func wantsDBXImport(args []string) bool {
	for i, a := range args {
		if a == "--from=databrickscfg" || (a == "--from" && i+1 < len(args) && args[i+1] == "databrickscfg") {
			return true
		}
	}
	return false
}

// cmdVaultImportDBX reads a databrickscfg once and creates vault provider rows; dry-run unless --apply.
func cmdVaultImportDBX(args []string) int {
	fs := helptext.NewFlagSet("vault import", helptext.Page{
		Usage: "claude-code vault import --from databrickscfg [flags]",
		About: "Create databricks provider rows from a CLI config, read once. Dry-run unless --apply.",
	})
	fs.String("from", "", "source format: databrickscfg")
	file := fs.String("file", "", "config path (default ~/.databrickscfg)")
	pick := fs.Bool("pick", false, "choose profiles in a picker (default: all)")
	apply := fs.Bool("apply", false, "write the rows (default: print the diff)")
	seal := fs.Bool("seal-secrets", false, "seal client secrets and PATs in the vault")
	replace := fs.Bool("replace", false, "overwrite an existing row of the same id")
	rename := renameFlag{}
	fs.Var(rename, "rename", "import profile OLD as row id NEW (repeatable)")
	if rest, err := outfmt.Parse(fs, args); err != nil {
		return helptext.ExitCode(err)
	} else if len(rest) > 0 {
		fs.Usage()
		return 2
	}
	path := *file
	if path == "" {
		path = dbximport.DefaultPath()
	}
	cands, err := dbximport.Candidates(path)
	if err != nil {
		return fail("vault import: %v", err)
	}
	if len(cands) == 0 {
		return fail("vault import: no profiles in %s", path)
	}
	if *pick {
		if cands, err = pickCandidates(cands); err != nil {
			return fail("vault import: %v", err)
		}
	}
	var rows []dbximport.Row
	oldDoc, newDoc, err := providers.UpdateOverlayDoc(false, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		rows = dbximport.Plan(cands, dbximport.Existing(top), dbximport.Options{Rename: rename, Replace: *replace})
		return dbximport.Apply(top, rows)
	})
	if err != nil {
		return fail("vault import: %v", err)
	}
	printImportTable(rows)
	if !profile.PrintDiff(os.Stdout, oldDoc, newDoc) {
		fmt.Println("no changes")
	}
	if !*apply {
		fmt.Println("dry-run: pass --apply to write")
		return 0
	}
	if _, _, err := providers.UpdateOverlayDoc(true, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		return dbximport.Apply(top, rows)
	}); err != nil {
		return fail("vault import: %v", err)
	}
	fmt.Println("wrote provider rows")
	if *seal {
		return sealImported(path, rows)
	}
	return 0
}

// sealImported stores each written row's client secret / PAT; the only place secrets are read.
func sealImported(path string, rows []dbximport.Row) int {
	code := 0
	for _, r := range rows {
		name := dbximport.SecretNameOf(r)
		if r.Action == "skip" || name == "" || !r.HasSecret {
			continue
		}
		s, err := dbximport.ReadSecrets(path, r.Name)
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

func printImportTable(rows []dbximport.Row) {
	fmt.Printf("%-32s %-16s %-46s %-6s %-7s %s\n", "PROFILE", "AUTHTYPE", "HOST", "SECRET", "ACTION", "NOTE")
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
		fmt.Printf("%-32s %-16s %-46s %-6s %-7s %s\n", id, kind, r.Host, has, r.Action, r.Reason)
	}
}

// pickCandidates toggles candidates in the arrow-key picker until Done; Esc cancels.
func pickCandidates(cands []dbximport.Candidate) ([]dbximport.Candidate, error) {
	on := make([]bool, len(cands))
	for i := range on {
		on[i] = true
	}
	at := 0
	for {
		rows := make([][2]string, 0, len(cands)+1)
		for i, c := range cands {
			mark := "[ ]"
			if on[i] {
				mark = "[x]"
			}
			rows = append(rows, [2]string{mark + " " + c.Name, c.Host})
		}
		rows = append(rows, [2]string{"Done", "import the checked profiles"})
		i, err := tui.Pick("Import profiles (Enter toggles)", rows, at)
		if err != nil {
			return nil, err
		}
		switch {
		case i < 0:
			return nil, fmt.Errorf("cancelled")
		case i == len(cands):
			var out []dbximport.Candidate
			for j, c := range cands {
				if on[j] {
					out = append(out, c)
				}
			}
			return out, nil
		}
		on[i], at = !on[i], i
	}
}
