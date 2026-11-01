package databricks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/tui"
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
	fold := fs.Bool("fold-legacy", false, "fold unreferenced digitalbtd user rows into dev/qa/prod by host")
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
		path = DefaultPath()
	}
	cands, err := Candidates(path)
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

	var existing map[string]json.RawMessage
	if _, _, err := providers.UpdateOverlayDoc(false, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		existing = Existing(top)
		return nil
	}); err != nil {
		return fail("import: %v", err)
	}
	choices, err := pickPairings(cands, existing, ImportOptions{Rename: rename, Replace: *replace})
	if err != nil {
		return fail("import: %v", err)
	}
	options := ImportOptions{Rename: rename, Replace: *replace, Pair: choices}
	var rows []Row
	var folded []string
	oldDoc, newDoc, err := providers.UpdateOverlayDoc(false, func(top map[string]json.RawMessage, tx *vault.Tx) error {
		rows = ImportPlan(cands, Existing(top), options)
		if err := Apply(top, rows); err != nil {
			return err
		}
		if *fold {
			folded, err = foldLegacy(top, tx)
			return err
		}
		return nil
	})
	if err != nil {
		return fail("import: %v", err)
	}
	printImportTable(rows)
	for _, id := range folded {
		fmt.Printf("fold %s -> %s\n", id, strings.TrimPrefix(id, legacyUserPrefix))
	}
	if !printDiff(os.Stdout, oldDoc, newDoc) {
		fmt.Println("no changes")
	}
	if len(folded) == 0 && !slices.ContainsFunc(rows, func(r Row) bool { return r.Action != "skip" }) {
		fmt.Println("nothing to import")
		return 0
	}
	if !*apply || *dryRun {
		fmt.Println("dry-run: pass --apply to write")
		return 0
	}
	if _, _, err := providers.UpdateOverlayDoc(true, func(top map[string]json.RawMessage, tx *vault.Tx) error {
		rows = ImportPlan(cands, Existing(top), options)
		if err := Apply(top, rows); err != nil {
			return err
		}
		if *fold {
			_, err := foldLegacy(top, tx)
			return err
		}
		return nil
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
func sealImported(path string, rows []Row) int {
	code := 0
	for _, r := range rows {
		name := SecretNameOf(r)
		if r.Action == "skip" || name == "" || !r.HasSecret {
			continue
		}
		source := r.Name
		if r.SPProfile != "" {
			source = r.SPProfile
		}
		s, err := ReadSecrets(path, source)
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

func printImportTable(rows []Row) {
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
func pickCandidates(cands []Candidate) ([]Candidate, error) {
	labels := make([][2]string, len(cands))
	for i, c := range cands {
		labels[i] = [2]string{c.Name, c.Host}
	}
	on, err := pickToggle("Import profiles (Enter toggles)", "import the checked profiles", labels)
	if err != nil {
		return nil, err
	}
	var out []Candidate
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
			return nil, errors.New("cancelled")
		case i == len(labels):
			return on, nil
		}
		on[i], at = !on[i], i
	}
}

// pickPairings leaves the engine deterministic and only asks about unresolved same-host sets.
func pickPairings(cands []Candidate, existing map[string]json.RawMessage, options ImportOptions) (map[string]string, error) {
	choices := map[string]string{}
	if !tui.StdinIsTerminal() {
		return choices, nil
	}
	plan := ImportPlan(cands, existing, options)
	seen := map[string]bool{}
	for _, sp := range plan {
		if sp.Auth != providers.AuthM2M || sp.Action == "skip" || sp.PairSet || seen[sp.Host] {
			continue
		}
		seen[sp.Host] = true
		var pairs [][2]Candidate
		for _, principal := range plan {
			if principal.Host != sp.Host || principal.Auth != providers.AuthM2M || principal.Action == "skip" || principal.PairSet {
				continue
			}
			for _, u := range plan {
				if u.Auth == providers.AuthOAuth && u.Action != "skip" && !u.PairingSet && u.Host == sp.Host {
					pairs = append(pairs, [2]Candidate{principal.Candidate, u.Candidate})
				}
			}
		}
		if len(pairs) == 0 {
			continue
		}
		match := func(p [2]Candidate) bool { return pairingEnv(p[0]) != "" && pairingEnv(p[0]) == pairingEnv(p[1]) }
		sort.SliceStable(pairs, func(i, j int) bool { return match(pairs[i]) && !match(pairs[j]) })
		labels := [][2]string{{"none", "leave these service principals unpaired"}}
		for _, p := range pairs {
			labels = append(labels, [2]string{p[1].Name + " <= SP " + p[0].Name, sp.Host})
		}
		at, err := tui.Pick("Pair a service principal", labels, 0)
		if err != nil {
			return nil, err
		}
		if at < 0 {
			return nil, errors.New("cancelled")
		}
		for _, p := range pairs {
			choices[p[0].Name] = ""
		}
		if at > 0 {
			p := pairs[at-1]
			choices[p[0].Name] = p[1].Name
		}
	}
	return choices, nil
}

const legacyUserPrefix = "digitalbtd-digitalbtd-user-"

// foldLegacy is explicit import work, never a registry-load migration; references always veto deletion.
func foldLegacy(top map[string]json.RawMessage, tx *vault.Tx) ([]string, error) {
	refs, err := legacyReferences(top, tx)
	if err != nil {
		return nil, err
	}
	provs := Existing(top)
	var folded []string
	for _, id := range []string{"dev", "qa", "prod"} {
		old := legacyUserPrefix + id
		if refs[old] || len(provs[old]) == 0 || len(provs[id]) == 0 {
			continue
		}
		var from, to providers.Provider
		if json.Unmarshal(provs[old], &from) != nil || json.Unmarshal(provs[id], &to) != nil {
			continue
		}
		host, ok := normalizeHost(providers.AuthHost(&from))
		dest, valid := normalizeHost(providers.AuthHost(&to))
		if !ok || !valid || host != dest || from.Preset != providers.PresetDatabricks || to.Preset != providers.PresetDatabricks {
			continue
		}
		if from.Auth.Type != to.Auth.Type || from.Auth.SPClientID != to.Auth.SPClientID || from.Identity != to.Identity {
			continue
		}
		delete(provs, old)
		folded = append(folded, old)
	}
	keys := providers.ObjectKeys(top["providers"])
	keys = slices.DeleteFunc(keys, func(k string) bool { _, ok := provs[k]; return !ok })
	top["providers"], err = providers.MarshalOrdered(keys, provs)
	return folded, err
}

// legacyReferences searches active vault configuration and plans; snapshots and a row's own key are not references.
func legacyReferences(top map[string]json.RawMessage, tx *vault.Tx) (map[string]bool, error) {
	refs := map[string]bool{}
	scan := func(text string) {
		for _, tag := range []string{"dev", "qa", "prod"} {
			id := legacyUserPrefix + tag
			if strings.Contains(text, id) {
				refs[id] = true
			}
		}
	}
	for key, raw := range top {
		if key != "providers" {
			scan(string(raw))
		}
	}
	for _, raw := range Existing(top) {
		scan(string(raw))
	}
	for _, bucket := range []string{vault.Pins, vault.Env, vault.Overlay} {
		for key, val := range tx.All(bucket) {
			if bucket == vault.Overlay && (key == "providers" || key == "providers.prev") {
				continue
			}
			scan(key)
			scan(val)
		}
	}
	dirs := []string{filepath.Join(paths.ConfigDir(), "plans")}
	for _, name := range []string{"settings.json", "settings.local.json"} {
		b, err := os.ReadFile(filepath.Join(paths.ConfigDir(), name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scan(string(b))
		var settings struct {
			PlansDirectory string `json:"plansDirectory"`
		}
		if err := json.Unmarshal(b, &settings); err != nil {
			return nil, err
		}
		if dir := settings.PlansDirectory; dir != "" {
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(paths.ConfigDir(), dir)
			}
			dirs = append(dirs, dir)
		}
	}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) && path == dir {
				return nil
			}
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scan(string(b))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return refs, nil
}
