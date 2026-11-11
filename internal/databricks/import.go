package databricks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
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
	dryRun := fs.Bool("dry-run", false, "print changes without writing, even with --apply")
	fold := fs.Bool("fold-legacy", false, "fold m2m rows into the profile pairing their host and client id")
	seal := fs.Bool("seal-secrets", false, "seal client secrets and PATs in the vault")
	replace := fs.Bool("replace", false, "overwrite an existing row of the same id")
	rename := renameFlag{}
	fs.Var(rename, "rename", "import section OLD as profile NEW (repeatable)")
	if rest, err := helptext.ParseArgs(fs, args); err != nil {
		return helptext.ExitCode(err)
	} else if len(rest) > 0 {
		fs.Usage()
		return 2
	}
	path := *file
	if path == "" {
		path = defaultPath()
	}
	cands, err := importCandidates(path, *pick)
	if err != nil {
		return fail("import: %v", err)
	}
	var existing map[string]json.RawMessage
	if _, _, err := providers.UpdateOverlayDoc(false, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		existing = existingRows(top)
		return nil
	}); err != nil {
		return fail("import: %v", err)
	}
	choices, err := pickPairings(cands, existing, importOptions{Rename: rename, Replace: *replace})
	if err != nil {
		return fail("import: %v", err)
	}
	options := importOptions{Rename: rename, Replace: *replace, Pair: choices}
	return runImport(path, cands, options, *apply && !*dryRun, *fold, *seal)
}

// importCandidates lists the profiles at path, narrowed by the picker when pick is set; each parser note is a warning.
func importCandidates(path string, pick bool) ([]cfgCandidate, error) {
	cands, notes, err := readCandidates(path)
	if err != nil {
		return nil, err
	}
	for _, n := range notes {
		fmt.Fprintf(os.Stderr, "claude-code databricks import: warning: %s\n", n)
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("no profiles in %s", path)
	}
	if pick {
		return pickCandidates(cands)
	}
	return cands, nil
}

func runImport(path string, cands []cfgCandidate, options importOptions, apply, fold, seal bool) int {
	rows, plan, oldDoc, newDoc, err := previewImport(cands, options, fold)
	if err != nil {
		return fail("import: %v", err)
	}
	printImportTable(rows)
	printFold(os.Stdout, plan)
	if !printDiff(os.Stdout, oldDoc, newDoc) {
		fmt.Println("no changes")
	}
	if plan.empty() && !slices.ContainsFunc(rows, func(r importRow) bool { return r.Action != "skip" }) {
		fmt.Println("nothing to import")
		return 0
	}
	if !apply {
		fmt.Println("dry-run: pass --apply to write")
		return 0
	}
	key, err := legacyFoldKey(plan)
	if err != nil {
		return fail("import: %v", err)
	}
	if rows, err = commitImport(cands, options, fold, key); err != nil {
		return fail("import: %v", err)
	}
	fmt.Println("wrote profiles")
	if seal {
		return sealImported(path, rows)
	}
	return 0
}

// previewImport plans the import and the legacy fold without writing; it returns the rows, the fold plan and both docs.
func previewImport(cands []cfgCandidate, options importOptions, fold bool) (
	rows []importRow, plan foldPlan, oldDoc, newDoc []byte, err error,
) {
	return planImport(cands, options, fold, false, nil)
}

func commitImport(cands []cfgCandidate, options importOptions, fold bool, key *vault.Key) ([]importRow, error) {
	rows, _, _, _, err := planImport(cands, options, fold, true, key)
	return rows, err
}

func planImport(cands []cfgCandidate, options importOptions, fold, write bool, key *vault.Key) (
	rows []importRow, plan foldPlan, oldDoc, newDoc []byte, err error,
) {
	oldDoc, newDoc, err = providers.UpdateOverlayDoc(write, func(top map[string]json.RawMessage, tx *vault.Tx) error {
		rows = importPlan(cands, existingRows(top), options)
		if err := applyRows(top, rows); err != nil {
			return err
		}
		if fold {
			var err error
			plan, err = foldLegacy(top, tx, key, write)
			return err
		}
		return nil
	})
	return rows, plan, oldDoc, newDoc, err
}

// sealImported stores each written row's client secret / PAT; the only place secrets are read.
func sealImported(path string, rows []importRow) int {
	code := 0
	for _, r := range rows {
		name := secretNameOf(r)
		if r.Action == "skip" || name == "" || !r.HasSecret {
			continue
		}
		if err := sealRow(path, r, name); err != nil {
			fmt.Fprintf(os.Stderr, "claude-code: seal %s: %v\n", name, err)
			code = 1
			continue
		}
		fmt.Printf("sealed %s\n", name)
	}
	return code
}

// sealRow reads r's secret from its source profile in the config at path and stores it in the vault as name.
func sealRow(path string, r importRow, name string) error {
	source := r.Name
	if r.SPProfile != "" {
		source = r.SPProfile
	}
	s, err := readSecrets(path, source)
	val := s.ClientSecret
	if r.Auth == providers.AuthPAT {
		val = s.Token
	}
	if err == nil && val == "" {
		err = errors.New("no secret in the source section")
	}
	if err == nil {
		err = vault.Store(name, val)
	}
	return err
}

func printImportTable(rows []importRow) {
	fmt.Printf("%-32s %-10s %-46s %-6s %-7s %s\n", "PROFILE", "AUTHTYPE", "HOST", "SECRET", "ACTION", "NOTE")
	for _, r := range rows {
		if r.SPProfile != "" {
			fmt.Printf("%s <= SP %s\n", r.ID, r.SPProfile)
		}
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
func pickCandidates(cands []cfgCandidate) ([]cfgCandidate, error) {
	labels := make([][2]string, len(cands))
	for i, c := range cands {
		labels[i] = [2]string{c.Name, c.Host}
	}
	on, err := pickToggle("Import profiles (Enter toggles)", "import the checked profiles", labels)
	if err != nil {
		return nil, err
	}
	var out []cfgCandidate
	for i, c := range cands {
		if on[i] {
			out = append(out, c)
		}
	}
	return out, nil
}

// Pick and Interactive are the arrow-key picker and terminal check, injected by internal/cli (see databricks_picker.go).
var (
	Pick        func(title string, rows [][2]string, start int) (int, error)
	Interactive func() bool
)

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
		i, err := Pick(title, rows, at)
		if err != nil {
			return nil, err
		}
		switch {
		case i < 0:
			return nil, errors.New("cancelled")
		case i == len(labels):
			return on, nil
		}
		on[i], at = !on[i], i
	}
}

// pickPairings leaves the engine deterministic and only asks about unresolved same-host sets.
func pickPairings(cands []cfgCandidate, existing map[string]json.RawMessage, options importOptions) (map[string]string, error) {
	choices := map[string]string{}
	if Interactive == nil || !Interactive() {
		return choices, nil
	}
	plan := importPlan(cands, existing, options)
	seen := map[string]bool{}
	for _, sp := range plan {
		if sp.Auth != providers.AuthM2M || sp.Action == "skip" || sp.PairSet || seen[sp.Host] {
			continue
		}
		seen[sp.Host] = true
		pairs := hostPairs(plan, sp.Host)
		if len(pairs) == 0 {
			continue
		}
		if err := pickHostPair(pairs, sp.Host, choices); err != nil {
			return nil, err
		}
	}
	return choices, nil
}

// hostPairs is every unresolved service principal × user pair on host.
func hostPairs(plan []importRow, host string) [][2]cfgCandidate {
	var pairs [][2]cfgCandidate
	for _, principal := range plan {
		if !openPrincipal(principal, host) {
			continue
		}
		for _, u := range plan {
			if openUser(u, host) {
				pairs = append(pairs, [2]cfgCandidate{principal.cfgCandidate, u.cfgCandidate})
			}
		}
	}
	return pairs
}

// openPrincipal is a service principal row on host that is still unpaired and not skipped.
func openPrincipal(r importRow, host string) bool {
	return r.Host == host && r.Auth == providers.AuthM2M && r.Action != "skip" && !r.PairSet
}

// openUser is a user (oauth) row on host that is still unpaired and not skipped.
func openUser(r importRow, host string) bool {
	return r.Auth == providers.AuthOAuth && r.Action != "skip" && !r.PairingSet && r.Host == host
}

// pickHostPair asks about one host's pairs, closest name match first, and records the answer in choices.
func pickHostPair(pairs [][2]cfgCandidate, host string, choices map[string]string) error {
	shared := func(p [2]cfgCandidate) int { // name tokens the SP and user share; more first
		n, user := 0, pairingTags(p[1])
		for _, t := range pairingTags(p[0]) {
			if slices.Contains(user, t) {
				n++
			}
		}
		return n
	}
	sort.SliceStable(pairs, func(i, j int) bool { return shared(pairs[i]) > shared(pairs[j]) })
	labels := [][2]string{{"none", "leave these service principals unpaired"}}
	for _, p := range pairs {
		labels = append(labels, [2]string{p[1].Name + " <= SP " + p[0].Name, host})
	}
	at, err := Pick("Pair a service principal", labels, 0)
	if err != nil {
		return err
	}
	if at < 0 {
		return errors.New("cancelled")
	}
	for _, p := range pairs {
		choices[p[0].Name] = ""
	}
	if at > 0 {
		p := pairs[at-1]
		choices[p[0].Name] = p[1].Name
	}
	return nil
}
