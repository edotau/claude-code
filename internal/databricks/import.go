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
		var err error
		if path, err = defaultPath(); err != nil {
			return fail("import: %v", err)
		}
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
	rows, plan, oldDoc, newDoc, err := planImport(cands, options, fold, false, nil)
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
	if rows, _, _, _, err = planImport(cands, options, fold, true, key); err != nil {
		return fail("import: %v", err)
	}
	fmt.Println("wrote profiles")
	if seal {
		return sealImported(path, rows)
	}
	return 0
}

// planImport plans the import and the legacy fold, writing only when write is set; returns rows, fold plan, docs.
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
	return pickSubset("Import profiles (Enter toggles)", "import the checked profiles", cands,
		func(c cfgCandidate) [2]string { return [2]string{c.Name, c.Host} })
}

// pickSubset runs pickToggle over items labelled by label and keeps the checked ones.
func pickSubset[T any](title, doneNote string, items []T, label func(T) [2]string) ([]T, error) {
	labels := make([][2]string, len(items))
	for i, it := range items {
		labels[i] = label(it)
	}
	on, err := pickToggle(title, doneNote, labels)
	if err != nil {
		return nil, err
	}
	var out []T
	for i, it := range items {
		if on[i] {
			out = append(out, it)
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
func pickPairings(cands []cfgCandidate, existing map[string]json.RawMessage, options importOptions) (map[string]string, error) {
	choices := map[string]string{}
	if !tui.StdinIsTerminal() {
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
	at, err := tui.Pick("Pair a service principal", labels, 0)
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

// importRow is the planned outcome for one candidate.
type importRow struct {
	cfgCandidate
	ID           string // vault provider id
	Action       string // add|replace|skip
	Reason       string // why skipped
	SPClientID   string // paired identifier, never the secret
	SPProfile    string // source section supplying the paired credential
	PairingSet   bool   // retain an explicit unpair as an empty spClientId
	identity     string
	previousHost string
	existed      bool
}

// importOptions steer importPlan.
type importOptions struct {
	Rename  map[string]string // source name to row id
	Replace bool
	Pair    map[string]string // SP section to user section; empty means explicitly unpaired
}

// importPlan maps candidates to rows against the existing provider rows (id to raw JSON).
func importPlan(cands []cfgCandidate, existing map[string]json.RawMessage, o importOptions) []importRow {
	rows := make([]importRow, 0, len(cands))
	taken := map[string]bool{}
	names := sealedNames(existing)
	for _, c := range cands {
		r, ek := baseImportRow(c, existing, o)
		if reason := skipReason(r, c, ek, o, taken, names); reason != "" {
			r.Action, r.Reason = "skip", reason
		} else if r.existed {
			r.Action = "replace"
		}
		if r.Action != "skip" {
			taken[r.ID] = true
			if n := secretNameOf(r); n != "" {
				names[n] = r.ID
			}
		}
		rows = append(rows, r)
	}
	return pairRows(rows, o.Pair, names)
}

// baseImportRow is candidate c's "add" row with the id, existing-row facts and oauth pairing carried over.
func baseImportRow(c cfgCandidate, existing map[string]json.RawMessage, o importOptions) (r importRow, ek string) {
	r = importRow{cfgCandidate: c, ID: c.Name, Action: "add"}
	if n, ok := o.Rename[c.Name]; ok {
		r.ID = n
	}
	raw, exists := existing[r.ID]
	ek = existingAuthType(raw)
	r.existed, r.previousHost = exists, existingHost(raw)
	if ek == providers.AuthOAuth {
		var p struct {
			Auth     map[string]json.RawMessage `json:"auth"`
			Identity string                     `json:"identity"`
		}
		if json.Unmarshal(raw, &p) != nil {
			return r, "" // a malformed row reads as unreadable, so the import skips it
		}
		value, set := p.Auth["spClientId"]
		if set && json.Unmarshal(value, &r.SPClientID) != nil {
			return r, ""
		}
		r.PairingSet, r.identity = set, p.Identity
	}
	return r, ek
}

// skipReason is why row r is not imported ("" = importable); the checks run in a fixed order.
func skipReason(r importRow, c cfgCandidate, ek string, o importOptions, taken map[string]bool, names map[string]string) string {
	switch {
	case c.Skip != "":
		return c.Skip
	case !providers.ValidID(r.ID):
		return "id must match [A-Za-z0-9_-]+ (use --rename)"
	case taken[r.ID]:
		return "duplicate id"
	}
	if r.existed {
		if reason := existingSkipReason(r, c, ek, o); reason != "" {
			return reason
		}
	}
	if r.SPClientID != "" && r.previousHost != c.Host {
		return "paired profile host is never changed on re-import"
	}
	if collides(names, r) {
		n := secretNameOf(r)
		return "secret name " + n + " is shared with row " + names[n] + "; use --rename"
	}
	return ""
}

// existingSkipReason is why an already-present row is left alone ("" = it may be replaced).
func existingSkipReason(r importRow, c cfgCandidate, ek string, o importOptions) string {
	switch {
	case !o.Replace:
		return "row exists (use --replace)"
	case ek == "":
		return "existing row has no readable sealed auth.type; switch it with `claude-code databricks login --profile " + r.ID + " --auth-type oauth`"
	case ek != c.Auth:
		return "existing auth.type " + ek + " is never changed"
	}
	return ""
}

// secretNameOf is the vault name of a planned row's sealed credential, derived by providers.
func secretNameOf(r importRow) string {
	if r.Auth == providers.AuthOAuth && r.SPClientID != "" {
		return secretName(r.ID, providers.AuthM2M)
	}
	return secretName(r.ID, r.Auth)
}

// secretName is the vault name of a databricks profile's sealed secret for an auth type.
func secretName(id, authType string) string {
	return providers.SecretName(providers.GroupDatabricks, id, authType)
}

// sealedNames maps each existing row's derived sealed-secret name to its id.
func sealedNames(existing map[string]json.RawMessage) map[string]string {
	out := map[string]string{}
	for id, raw := range existing {
		var p struct {
			Auth struct {
				SPClientID string `json:"spClientId"`
			} `json:"auth"`
		}
		_ = json.Unmarshal(raw, &p) // an unrelated bad field must not hide the row's sealed secret
		auth := existingAuthType(raw)
		if auth == providers.AuthOAuth && p.Auth.SPClientID != "" {
			out[secretName(id, providers.AuthM2M)] = id
		}
		if n := secretName(id, auth); n != "" {
			out[n] = id
		}
	}
	return out
}

// collides reports whether r's secret name already belongs to a different row.
func collides(names map[string]string, r importRow) bool {
	n := secretNameOf(r)
	o, ok := names[n]
	return n != "" && ok && o != r.ID
}

// existingAuthType is an existing row's sealed auth.type (oauth, m2m, pat); "" for any other row.
func existingAuthType(raw json.RawMessage) string {
	var p struct {
		Auth struct {
			Type string `json:"type"`
		} `json:"auth"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	if !slices.Contains([]string{providers.AuthOAuth, providers.AuthM2M, providers.AuthPAT}, p.Auth.Type) {
		return ""
	}
	return p.Auth.Type
}

// rowJSON renders the vault row for a planned candidate.
func rowJSON(r importRow) (json.RawMessage, error) {
	auth := map[string]any{"type": r.Auth}
	if r.Auth == providers.AuthM2M {
		auth["clientId"] = r.ClientID
	}
	if r.AccountID != "" {
		auth["accountId"] = r.AccountID
	}
	if r.WorkspaceID != "" {
		auth["workspaceId"] = r.WorkspaceID
	}
	if r.PairingSet && r.Auth == providers.AuthOAuth {
		auth["spClientId"] = r.SPClientID
	}
	out := map[string]any{"preset": "databricks", "host": r.Host, "auth": auth}
	if r.identity != "" {
		out["identity"] = r.identity
	}
	return json.Marshal(out)
}

// applyRows writes the add/replace rows into the overlay doc's providers map.
func applyRows(top map[string]json.RawMessage, rows []importRow) error {
	if raw, ok := top["providers"]; ok {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return fmt.Errorf("providers overlay: existing value is not an object: %w", err)
		}
	}
	provs := existingRows(top)
	keys := providers.ObjectKeys(top["providers"])
	for _, r := range rows {
		if r.Action == "skip" {
			continue
		}
		raw, err := rowJSON(r)
		if err != nil {
			return err
		}
		if _, ok := provs[r.ID]; !ok {
			keys = append(keys, r.ID)
		}
		provs[r.ID] = raw
	}
	b, err := providers.MarshalOrdered(keys, provs)
	if err != nil {
		return fmt.Errorf("providers overlay: %w", err)
	}
	top["providers"] = b
	return nil
}

// existingRows returns the overlay's provider rows by id.
func existingRows(top map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if raw, ok := top["providers"]; ok {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// pairRows consumes explicit choices before same-host inference; any existing user pairing decision is final.
func pairRows(rows []importRow, choices map[string]string, names map[string]string) []importRow {
	for _, i := range pairOrder(rows, choices) {
		sp := &rows[i]
		if sp.Auth != providers.AuthM2M || sp.Skip != "" {
			continue
		}
		users, countSP := sameHostRows(rows, *sp)
		target, explicit := pairTarget(rows, *sp, users, countSP, choices)
		paired := attachToTarget(rows, users, target, sp, names)
		if !paired && len(users) > 0 && !explicit && sp.Action != "skip" {
			sp.Reason = "unpaired; choose a service principal in configure"
		}
	}

	markDeclinedPairings(rows, choices)
	return rows
}

// attachToTarget attaches sp to the same-host user row named target, reporting whether one took it.
func attachToTarget(rows []importRow, users []int, target string, sp *importRow, names map[string]string) bool {
	for _, j := range users {
		if rows[j].Name == target && attachSP(&rows[j], sp, names) {
			return true
		}
	}
	return false
}

// pairOrder is rows' indexes with explicit choices and saved pairings first, otherwise stable.
func pairOrder(rows []importRow, choices map[string]string) []int {
	order := make([]int, len(rows))
	for i := range rows {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int {
		_, a := choices[rows[i].Name]
		_, b := choices[rows[j].Name]
		a, b = a || rows[i].PairSet, b || rows[j].PairSet
		if a == b {
			return 0
		}
		if a {
			return -1
		}
		return 1
	})
	return order
}

// markDeclinedPairings records an explicit "no pairing" on the same-host users still without a service principal.
func markDeclinedPairings(rows []importRow, choices map[string]string) {
	for _, sp := range rows {
		target, explicit := choices[sp.Name]
		if !explicit {
			target, explicit = sp.Pair, sp.PairSet
		}
		if sp.Auth != providers.AuthM2M || !explicit || target != "" {
			continue
		}
		for j := range rows {
			u := &rows[j]
			if u.Auth == providers.AuthOAuth && u.Host == sp.Host && u.SPClientID == "" {
				u.PairingSet = true
			}
		}
	}
}

// pairingTags are a candidate's Env plus its name tokens; they only order choices, never override host safety.
func pairingTags(c cfgCandidate) []string {
	tags := strings.FieldsFunc(strings.ToLower(c.Name), func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	if c.Env != "" {
		tags = append(tags, strings.ToLower(c.Env))
	}
	return tags
}

// existingHost reads a row without expanding or redacting it.
func existingHost(raw json.RawMessage) string {
	var p providers.Provider
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	host, _ := normalizeHost(providers.AuthHost(&p))
	return host
}

// sameHostRows counts source profiles, including ones that an earlier pairing already consumed.
func sameHostRows(rows []importRow, sp importRow) (users []int, principals int) {
	for j, u := range rows {
		if u.Skip != "" || u.Host != sp.Host {
			continue
		}
		if u.Auth == providers.AuthM2M {
			principals++
		}
		if u.Auth == providers.AuthOAuth {
			users = append(users, j)
		}
	}
	return users, principals
}

func pairTarget(rows []importRow, sp importRow, users []int, principals int, choices map[string]string) (string, bool) {
	if target, ok := choices[sp.Name]; ok {
		return target, true
	}
	if sp.PairSet {
		return sp.Pair, true
	}
	for _, j := range users {
		if rows[j].SPClientID == sp.ClientID && rows[j].previousHost == sp.Host {
			return rows[j].Name, false
		}
	}
	if len(users) == 1 && principals == 1 {
		return rows[users[0]].Name, false
	}
	return "", false
}

// canAttachSP is the row-state half of attachSP's guard: no stored decision or foreign credential is overridden.
func canAttachSP(u, sp *importRow) bool {
	if u.Action == "skip" && (!u.existed || u.SPClientID != sp.ClientID) {
		return false
	}
	if u.PairingSet && (u.SPClientID == "" || u.SPClientID != sp.ClientID || u.previousHost != sp.Host) {
		return false
	}
	return u.SPProfile == "" && (u.Action == "skip" || sp.Action != "skip")
}

// attachSP never replaces a stored pairing decision or borrows a credential from a different host.
func attachSP(u, sp *importRow, names map[string]string) bool {
	if !canAttachSP(u, sp) {
		return false
	}
	name := secretName(u.ID, providers.AuthM2M)
	if owner := names[name]; owner != "" && owner != u.ID {
		return false
	}
	u.SPClientID, u.SPProfile, u.PairingSet, u.HasSecret = sp.ClientID, sp.Name, true, sp.HasSecret
	names[name] = u.ID
	sp.Action, sp.Reason = "skip", "paired with "+u.ID
	return true
}
