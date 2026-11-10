package menu

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// pickKey is the terminal picker seam; tests replace it.
var pickKey = tui.PickBack

// keysMenu lists every credential with its status; picking one opens its Login / OAuth token / API key actions
// (or the set-up flow when absent), then returns to the list. Back leaves. Nothing changes unless a row is picked.
func keysMenu(d *dialog) (allOK, changed bool) {
	start, stale := 0, true
	var targets []configure.KeyTarget
	var rows [][2]string
	var ok bool
	for {
		if stale { // re-probe only after a change: each probe can take seconds
			var err error
			if targets, err = configure.KeyTargets(d.reg, d.o.Agent); err != nil {
				d.println(err)
				return false, changed
			}
			rows, ok = keyRows(targets)
		}
		idx, err := pickKey("keys — pick a credential to change, or Back to leave", rows, start)
		if err != nil || idx < 0 {
			return ok, changed
		}
		start = idx
		stale = changeKey(d, targets[idx])
		changed = changed || stale
	}
}

// keyRows renders one picker row per credential (name, then mode and check aligned) and reports whether every
// credential is present and accepted.
func keyRows(targets []configure.KeyTarget) ([][2]string, bool) {
	names := make([]string, len(targets))
	cols := make([][2]string, len(targets))
	allOK := true
	var wg sync.WaitGroup
	for i, kt := range targets {
		names[i] = kt.Label
		wg.Go(func() { cols[i] = keyCols(kt) }) // probes run in parallel, not one 4s cap after another
	}
	wg.Wait()
	for _, c := range cols {
		allOK = allOK && c[1] == "verified"
	}
	return WithChecks(names, cols), allOK
}

// keyCols is one credential's mode (how it signs in · how it bills) and check: verified, unverified, rejected or
// missing. A login counts as verified when its own CLI's status check confirmed it.
func keyCols(kt configure.KeyTarget) [2]string {
	if !kt.Present {
		return [2]string{"not set up", "not verified"}
	}
	if kt.Provider != nil && kt.EnvVar != "" {
		status, _ := configure.Probe(kt)
		if status == "verified" {
			return [2]string{modeAPIKey, "verified"}
		}
		if status == "login" {
			return [2]string{loginLine(kt), "verified"}
		}
		return [2]string{modeAPIKey, "not verified"}
	}
	return [2]string{loginLine(kt), "verified"}
}

var tierRE = regexp.MustCompile(`\((\w+)\)`)

// loginLine is a login credential's mode: "subscription", with the tier when the CLI reports one ("subscription (max)").
func loginLine(kt configure.KeyTarget) string {
	if kt.Label == "claude" {
		if m := tierRE.FindStringSubmatch(kt.Source); m != nil {
			return "subscription (" + m[1] + ")"
		}
	}
	return "subscription"
}

// changeKey runs the picked credential's actions: rotate when present, set up when absent.
func changeKey(d *dialog, kt configure.KeyTarget) bool {
	if !kt.Present {
		if !actionConfirmed("Set up " + kt.Label) {
			return false
		}
		return resolveKey(d, kt)
	}
	if len(configure.RotateOptions(kt)) == 0 {
		d.printf("%-10s nothing to rotate here: %s\n", kt.Label, kt.Source)
		return false
	}
	return runRotate(d, kt)
}

// replaceKey pastes a new value for kt's keyed credential, probes it live before it overwrites the
// stored one (a rejected/unreachable paste asks whether to keep the old one instead), and writes it
// back to the vault.
func replaceKey(d *dialog, kt configure.KeyTarget) bool {
	if kt.EnvVar == "" {
		return false
	}
	if _, src, _ := vault.Get(kt.EnvVar); src == "env" {
		d.printf("%s: env-only, not in the vault; edit where exported\n", kt.EnvVar)
		return false
	}
	if !actionConfirmed("Paste a new " + kt.EnvVar + " to rotate it") {
		return false
	}
	if u := configure.ConsoleURLs[kt.EnvVar]; u != "" && d.shell().Confirm("Open "+u+" in your browser to create the new key?") {
		openBrowser(u)
	}
	val, err := secretLine(d, "Paste the new value for "+kt.EnvVar+" (input hidden, Enter when done): ")
	if err != nil || strings.TrimSpace(val) == "" {
		d.printf("%s: no value entered\n", kt.EnvVar)
		return false
	}
	_, v := configure.ParseAssignment(val)
	if keepOldKey(d, kt, v) {
		return false
	}
	return storeValue(d, kt.EnvVar, v)
}

// keepOldKey probes the pasted value v live; true when it is rejected or unreachable and the user keeps the old key.
func keepOldKey(d *dialog, kt configure.KeyTarget, v string) bool {
	if kt.Provider == nil {
		return false
	}
	status, detail := configure.ProbeValue(kt.Provider, kt.EnvVar, v)
	if status != "rejected" && status != "unreachable" {
		return false
	}
	d.printf("%s: new value %s (%s)\n", kt.EnvVar, status, detail)
	return confirmKeep(d, "Keep the old key and discard this paste?")
}

// confirmKeep asks question [Y/n]; a blank or y answer keeps the old key (true), and ended input keeps it too.
func confirmKeep(d *dialog, question string) bool {
	ans, ok := readAnswer(d, question+" [Y/n] ")
	if !ok {
		d.println("input ended: kept the old key")
		return true
	}
	return !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "n")
}

// readAnswer reads one line after prompt from the same reader the menu's prompts use; ok is false when the
// input ended or failed, so the caller never mistakes a dead input for an empty answer.
func readAnswer(d *dialog, prompt string) (string, bool) {
	fmt.Fprint(d.out, prompt)
	br := d.br
	if d.shell().TTY {
		br = tui.Stdin()
	}
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(d.out)
		return "", false
	}
	return strings.TrimRight(line, "\r\n"), true
}

// runMint asks first (a stray Enter must never start a mint), then runs the token-minting CLI.
func runMint(argv []string) (string, error) {
	if !actionConfirmed("Run `" + strings.Join(argv, " ") + "` to mint a new token") {
		return "", nil
	}
	return configure.RunMint(argv)
}

// regenerate re-runs an agent's own login or mint CLI to rotate a credential that is already present.
// A login reports the outcome (the session lives in the agent's own file, not the vault).
// A mint writes the printed token back to the vault.
func regenerate(d *dialog, kt configure.KeyTarget, argv []string, mint bool) bool {
	if !mint {
		if err := configure.RunLogin(argv); err != nil {
			d.printf("%s: %v\n", kt.Label, err)
			return false
		}
		d.printf("%-10s logged in again\n", kt.Label)
		return true
	}
	tok, err := runMint(argv)
	if err != nil {
		d.printf("%s: %v\n", kt.Label, err)
		return false
	}
	if tok == "" {
		d.printf("%s: no token found in the output; try again or paste one\n", kt.EnvVar)
		return false
	}
	if _, src, _ := vault.Get(kt.EnvVar); src == "env" {
		d.printf("%s: minted a new token, saving it to the vault; the exported %s still overrides it\n", kt.Label, kt.EnvVar)
	}
	return storeValue(d, kt.EnvVar, tok)
}

// runRotate offers kt's rotate options (generate first, else paste) and runs the chosen one; false
// means skipped, cancelled, or nothing to rotate.
func runRotate(d *dialog, kt configure.KeyTarget) bool {
	opts := configure.RotateOptions(kt)
	if len(opts) == 0 {
		return false
	}
	chosen := 0
	if len(opts) > 1 {
		var ok bool
		if chosen, ok = chooseRotateOption(d, kt, opts); !ok {
			return false
		}
	}
	o := opts[chosen]
	switch {
	case len(o.Argv) > 0:
		return regenerate(d, kt, o.Argv, false)
	case len(o.Mint) > 0:
		return regenerate(d, kt, o.Mint, true)
	default:
		return replaceKey(d, kt)
	}
}

// chooseRotateOption picks among several rotate options: a picker on a terminal, else a confirm on the first
// (no selects the second); ok is false when the picker is cancelled.
func chooseRotateOption(d *dialog, kt configure.KeyTarget, opts []configure.AuthOption) (chosen int, ok bool) {
	if !d.onTerminal() {
		if d.shell().Confirm(opts[0].Label() + "?") {
			return 0, true
		}
		return 1, true
	}
	rows := make([]ShellRow, len(opts))
	for i, o := range opts {
		rows[i] = ShellRow{Name: o.Action, Detail: o.Desc}
	}
	idx, err := d.shell().Pick(kt.Label+" — rotate which credential?", Pairs(rows))
	if err != nil || idx < 0 {
		return 0, false
	}
	return idx, true
}

// ensureKey makes sure kt's keyed credential is usable before a sub-picker pins it: absent asks to add
// one, present defaults to keeping it and only replaces on an explicit no.
func ensureKey(d *dialog, kt configure.KeyTarget, present bool) bool {
	if !present {
		return resolveKey(d, kt)
	}
	if d.shell().Confirm(kt.Label + " key present — keep it?") {
		return true
	}
	return replaceKey(d, kt)
}

// sourceLabel names the effective source and flags an env override of a vault value.
func sourceLabel(src, shadowed string) string {
	switch {
	case src == "":
		return "missing"
	case src == "unreadable":
		return "vault unreadable"
	case src == "env" && shadowed == "vault":
		return "env over vault"
	}
	return src
}

// Secrets is the guided `vault set`: pick a tracked secret (or a new custom one), choose how to satisfy it,
// and seal it in the vault. Loops until Esc or done.
func Secrets(o Options) int {
	reg, err := providers.Load()
	if err != nil {
		fmt.Fprintf(o.writer(), "secrets: %v\n", err)
		return 1
	}
	o.In = o.reader()
	d := &dialog{o: o, reg: reg, br: bufio.NewReader(o.In), out: o.writer(), color: ansi.Enabled(o.writer())}
	return secretsLoop(d)
}

// secretsLoop is the secrets page on d: standalone it stores at once; inside configure (d.pending set) every
// value is staged and configure's Submit saves it to the vault.
func secretsLoop(d *dialog) int {
	if d.pending != nil {
		before := maps.Clone(d.pending)
		defer confirmPageKeys(d, before)
	}
	reg := d.reg
	store := "the vault"
	for {
		list := configure.TrackedSecrets(reg)
		rows := make([][2]string, 0, len(list)+2)
		for _, s := range list {
			rows = append(rows, [2]string{s.Name, fmt.Sprintf("%-16s %s", sourceLabel(s.Source, s.Shadowed), s.Users)})
		}
		rows = append(rows, [2]string{"+ New", "custom NAME, then paste"}, EnterRow("save and go back"))
		title := "secrets — pick one to set (stores in " + store + ")"
		if d.pending != nil {
			title = "secrets — pick one to set (Submit saves to " + store + ")"
		}
		idx, err := d.shell().PickPlain(title, rows)
		if err != nil {
			fmt.Fprintf(d.out, "secrets: %v\n", err)
			return 1
		}
		if idx < 0 || idx == len(rows)-1 {
			return 0
		}
		if idx == len(list) {
			addCustom(d)
			continue
		}
		editSecret(d, list[idx])
	}
}

// editSecret offers mint, login, paste, and vault deletion actions for one tracked secret.
func editSecret(d *dialog, s configure.TrackedSecret) {
	var acts []ShellRow
	kt := configure.KeyTarget{Label: s.Name, EnvVar: s.Name, FixCmd: "claude-code vault set " + s.Name}
	if s.Name == providers.OAuthTokenEnv {
		kt.MintArgv = []string{"claude", "setup-token"}
	}
	for _, o := range configure.AuthOptions(kt) {
		acts = append(acts, ShellRow{Name: o.Action, Detail: o.Desc, Run: func() bool { return runAuthOption(d, kt, o) }})
	}
	if s.Source == "vault" {
		acts = append(acts, ShellRow{Name: "Delete", Detail: "remove from the vault", Run: func() bool { return deleteSecret(d, s.Name) }})
	}
	idx, err := d.shell().Pick(s.Name+" ("+sourceLabel(s.Source, s.Shadowed)+")", Pairs(acts))
	if err != nil || idx < 0 || idx >= len(acts) {
		return
	}
	acts[idx].Run()
}

// deleteSecret confirms, then deletes name from the vault, or stages the delete inside the configure menu.
func deleteSecret(d *dialog, name string) bool {
	if !d.shell().Confirm("Delete " + name + "?") {
		return false
	}
	if d.pending != nil {
		d.stageDelete(name)
		d.printf("staged delete of %s · Submit removes it from the vault\n", name)
		return true
	}
	if err := configure.DeleteSecret(name); err != nil {
		d.printf("%s: %v\n", name, err)
		return false
	}
	d.printf("deleted %s\n", name)
	return true
}

// addCustom reads a NAME (visible) and its value (hidden); a pasted NAME=value or `export NAME="value"` line is
// parsed, so either half may carry the name.
func addCustom(d *dialog) {
	name := strings.TrimSpace(d.shell().Line("Secret name (e.g. NEW_SECRET; blank to take it from the pasted line): "))
	raw, err := secretLine(d, "Paste the value or a NAME=value line (input hidden, Enter when done): ")
	if err != nil {
		d.printf("secrets: %v\n", err)
		return
	}
	pn, val := configure.ParseAssignment(raw)
	if name == "" {
		name = pn
	}
	switch {
	case !vault.EnvName.MatchString(name):
		d.printf("secrets: %q is not a valid name (letters, digits, underscore; not starting with a digit)\n", name)
	case val == "":
		d.printf("%s: no value entered\n", name)
	default:
		storeValue(d, name, val)
	}
}

// storeValue writes one secret to the vault and reports it redacted; inside the menu it stages.
func storeValue(d *dialog, name, val string) bool {
	if err := configure.ValidateSecret(name, val); err != nil {
		d.printf("%v\n", err)
		return false
	}
	if d.pending != nil {
		if !d.stage(name, val) {
			return false
		}
		delete(d.pendingDeletes, name) // re-picking a staged delete to paste a new value must cancel the delete
		d.printf("staged %s (%s) · Submit saves it to the secrets manager\n", name, vault.Redact(val))
		return true
	}
	if err := configure.StoreSecret(name, val); err != nil {
		d.printf("%s: %v\n", name, err)
		return false
	}
	d.printf("wrote %s to the vault (%s)\n", name, vault.Redact(val))
	return true
}

// confirmPageKeys asks, on leaving the secrets page inside configure, whether to save what the page staged
// now (y) or drop it (N); keys staged before the page opened are left for configure's own Submit. Staged
// deletes (editSecret's Delete, in staged mode) are decided in the same prompt.
func confirmPageKeys(d *dialog, before map[string]stagedKey) {
	page := changedPageKeys(d, before)
	deletes := slices.Sorted(maps.Keys(d.pendingDeletes))
	if len(page) == 0 && len(deletes) == 0 {
		return
	}
	n := len(page) + len(deletes)
	for _, name := range deletes {
		if slices.Contains(page, name) {
			n-- // a key both staged and staged-for-delete is one change
		}
	}
	if !confirmLeave(n, "change(s) to the secrets manager") {
		for _, name := range page {
			restoreStaged(d, name, before)
		}
		d.pendingDeletes = nil
		d.printf("secrets: %d change(s) discarded\n", n)
		return
	}
	for _, name := range deletes {
		if _, staged := d.pending[name]; staged {
			restoreStaged(d, name, map[string]stagedKey{}) // the delete wins over any staged Store of the same name
		}
	}
	for _, name := range page {
		k, ok := d.pending[name]
		if !ok {
			continue
		}
		if err := configure.StoreSecret(name, k.val); err != nil {
			d.printf("%s: not saved: %v\n", name, err)
			continue
		}
		d.printf("saved %s to the vault (%s)\n", name, vault.Redact(k.val))
		delete(d.pending, name) // stored: the env it exported stays, Submit has nothing left to do for it
	}
	d.pendingDeletes = nil
	for _, name := range deletes {
		if err := configure.DeleteSecret(name); err != nil {
			d.printf("%s: not deleted: %v\n", name, err)
			d.stageDelete(name) // stays pending: a failed delete is retried, and fails the menu, not dropped
			continue
		}
		d.printf("deleted %s\n", name)
	}
	d.changed = true
}

// stageDelete queues name for vault deletion on Submit (or the page's exit).
func (d *dialog) stageDelete(name string) {
	if d.pendingDeletes == nil {
		d.pendingDeletes = map[string]bool{}
	}
	d.pendingDeletes[name] = true
}

// changedPageKeys lists, sorted, the keys staged during this page: not as before it, and not the value already stored.
func changedPageKeys(d *dialog, before map[string]stagedKey) []string {
	var page []string
	for name, k := range d.pending {
		if prev, ok := before[name]; ok && prev.val == k.val {
			continue
		}
		if v, _, ok := vault.Get(name); ok && v == k.val && k.had && k.prev == v {
			continue // re-entered the value already stored: not a change
		}
		page = append(page, name)
	}
	slices.Sort(page)
	return page
}

// restoreStaged undoes one page-staged key: back to its pre-page staging, or out of pending with its env restored.
func restoreStaged(d *dialog, name string, before map[string]stagedKey) {
	if prev, ok := before[name]; ok {
		d.pending[name] = prev
		_ = os.Setenv(name, prev.val)
		return
	}
	k := d.pending[name]
	if k.had {
		_ = os.Setenv(name, k.prev)
	} else {
		_ = os.Unsetenv(name)
	}
	delete(d.pending, name)
}
