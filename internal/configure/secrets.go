package configure

import (
	"bufio"
	"cmp"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tailscale"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

// envName is a valid secret name: an env var identifier.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// trackedSecret is one row of the secrets dialog: a name the harness knows of and where it resolves today.
type trackedSecret struct {
	name     string
	source   string // vault|env|"" (missing)
	shadowed string // vault|"" — a stored value an env var is shadowing
	users    string // providers that read it, or a short role
}

// trackedSecrets combines provider auth.env names, the setup-token, and every secret stored in the vault.
func trackedSecrets(reg *providers.Registry) []trackedSecret {
	users := map[string][]string{
		providers.OAuthTokenEnv: {"claude -p (setup-token)"},
	}
	for _, n := range reg.Names() {
		if env := reg.Providers[n].Auth.Env; env != "" {
			users[env] = append(users[env], n)
		}
	}
	meta, metaErr := secrets.LoadMeta()
	if metaErr != nil {
		// Names reads vault names without decoding records, which may still work when metadata is unreadable.
		for _, env := range secrets.Names() {
			if _, ok := users[env]; !ok {
				users[env] = []string{"custom"}
			}
		}
	} else {
		for env := range meta {
			if _, ok := users[env]; !ok {
				users[env] = []string{"custom"}
			}
		}
	}
	out := make([]trackedSecret, 0, len(users))
	for name, u := range users {
		var src, shadowed string
		if metaErr == nil {
			src, shadowed = secrets.Locate(name, meta)
		} else {
			_, src, shadowed, _ = secrets.Effective(name)
		}
		out = append(out, trackedSecret{name: name, source: src, shadowed: shadowed, users: strings.Join(u, ", ")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// sourceLabel names the effective source and flags an env override of a vault value.
func sourceLabel(src, shadowed string) string {
	switch {
	case src == "":
		return "missing"
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
		list := trackedSecrets(reg)
		rows := make([][2]string, 0, len(list)+2)
		for _, s := range list {
			rows = append(rows, [2]string{s.name, fmt.Sprintf("%-16s %s", sourceLabel(s.source, s.shadowed), s.users)})
		}
		rows = append(rows, [2]string{"+ New", "custom NAME, then paste"}, enterRow("save and go back"))
		title := "secrets — pick one to set (stores in " + store + ")"
		if d.pending != nil {
			title = "secrets — pick one to set (Submit saves to " + store + ")"
		}
		idx, err := pickPlain(d, title, rows)
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

// pick is tui.Pick on the controlling terminal, a numbered prompt otherwise (tests, pipes).
// start, if given, is the initial cursor row (TUI path only; the numbered-prompt path ignores it).
func pick(d *dialog, title string, rows [][2]string, start ...int) (int, error) {
	if isStdinTTY() && d.o.In == os.Stdin {
		return tui.PickBack(title, rows, cmp.Or(start...))
	}
	d.println(title)
	for i, r := range rows {
		d.printf("  %d) %-22s %s\n", i+1, r[0], r[1])
	}
	fmt.Fprint(d.out, "pick a number (blank = cancel): ")
	n, err := strconv.Atoi(strings.TrimSpace(readLine(d.br)))
	if err != nil || n < 1 || n > len(rows) {
		return -1, nil
	}
	return n - 1, nil
}

// editSecret offers mint, login, paste, and vault deletion actions for one tracked secret.
func editSecret(d *dialog, s trackedSecret) {
	type action struct {
		name, desc string
		run        func() bool
	}
	var acts []action
	kt := keyTarget{label: s.name, envVar: s.name, fixCmd: "claude-code vault set " + s.name}
	if s.name == providers.OAuthTokenEnv {
		kt.mintArgv = []string{"claude", "setup-token"}
	}
	for _, o := range authOptions(kt) {
		acts = append(acts, action{o.action, o.desc, func() bool { return runAuthOption(d, kt, o) }})
	}
	if s.source == "vault" {
		acts = append(acts, action{"Delete", "remove from the vault", func() bool {
			if !confirm(d, "Delete "+s.name+"?") {
				return false
			}
			if d.pending != nil {
				if _, staged := d.pending[s.name]; staged {
					restoreStaged(d, s.name, map[string]stagedKey{}) // drop any staged Store of the same name
				}
				if d.pendingDeletes == nil {
					d.pendingDeletes = map[string]bool{}
				}
				d.pendingDeletes[s.name] = true
				d.printf("staged delete of %s · Submit removes it from the vault\n", s.name)
				return true
			}
			if err := secrets.Delete(s.name); err != nil {
				d.printf("%s: %v\n", s.name, err)
				return false
			}
			audit.Log(audit.SecretDelete, s.name, "")
			d.printf("deleted %s\n", s.name)
			return true
		}})
	}
	rows := make([][2]string, 0, len(acts))
	for _, a := range acts {
		rows = append(rows, [2]string{a.name, a.desc})
	}
	idx, err := pick(d, s.name+" ("+sourceLabel(s.source, s.shadowed)+")", rows)
	if err != nil || idx < 0 || idx >= len(acts) {
		return
	}
	acts[idx].run()
}

// addCustom reads a NAME (visible) and its value (hidden); a pasted NAME=value or `export NAME="value"` line is
// parsed, so either half may carry the name.
func addCustom(d *dialog) {
	name := strings.TrimSpace(textLine(d, "Secret name (e.g. NEW_SECRET; blank to take it from the pasted line): "))
	raw, err := secretLine(d, "Paste the value or a NAME=value line (input hidden, Enter when done): ")
	if err != nil {
		d.printf("secrets: %v\n", err)
		return
	}
	pn, val := ParseAssignment(raw)
	if name == "" {
		name = pn
	}
	switch {
	case !envName.MatchString(name):
		d.printf("secrets: %q is not a valid name (letters, digits, underscore; not starting with a digit)\n", name)
	case val == "":
		d.printf("%s: no value entered\n", name)
	default:
		storeValue(d, name, val)
	}
}

// ParseAssignment splits a pasted secret: `export NAME="value"`, `NAME=value`, or a bare value (name "").
// Surrounding quotes and whitespace are stripped; a bare value containing '=' (base64 padding) stays whole.
func ParseAssignment(raw string) (name, value string) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSpace(strings.TrimPrefix(s, "export "))
	if k, v, ok := strings.Cut(s, "="); ok && v != "" && envName.MatchString(k) && strings.ToUpper(k) == k {
		name, s = k, strings.TrimSpace(v)
	}
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	return name, s
}

// storeValue writes one secret to the vault and reports it redacted; inside the menu it stages.
func storeValue(d *dialog, name, val string) bool {
	if name == vendors.CopilotTokenEnv && !vendors.ValidCopilotToken(val) {
		d.printf("%s: not a GitHub token (want gho_/ghp_/ghu_/github_pat_, >=20 chars)\n", name)
		return false
	}
	if err := tailscale.ValidCredential(name, val); err != nil {
		d.printf("%v\n", err)
		return false
	}
	if d.pending != nil {
		if !d.stage(name, val) {
			return false
		}
		delete(d.pendingDeletes, name) // re-picking a staged delete to paste a new value must cancel the delete
		d.printf("staged %s (%s) · Submit saves it to the secrets manager\n", name, secrets.Redact(val))
		return true
	}
	if err := secrets.Store(name, val); err != nil {
		d.printf("%s: %v\n", name, err)
		return false
	}
	d.printf("wrote %s to the vault (%s)\n", name, secrets.Redact(val))
	return true
}

// textLine reads one visible line: straight from the terminal like tui.Confirm, the dialogue reader otherwise.
func textLine(d *dialog, prompt string) string {
	fmt.Fprint(d.out, prompt)
	if isStdinTTY() && d.o.In == os.Stdin {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimRight(line, "\r\n")
	}
	return readLine(d.br)
}

// confirmPageKeys asks, on leaving the secrets page inside configure, whether to save what the page staged
// now (y) or drop it (N); keys staged before the page opened are left for configure's own Submit. Staged
// deletes (editSecret's Delete, in staged mode) are decided in the same prompt.
func confirmPageKeys(d *dialog, before map[string]stagedKey) {
	var page []string
	for name, k := range d.pending {
		if prev, ok := before[name]; ok && prev.val == k.val {
			continue
		}
		if v, _, ok := secrets.Get(name); ok && v == k.val && k.had && k.prev == v {
			continue // re-entered the value already stored: not a change
		}
		page = append(page, name)
	}
	slices.Sort(page)
	deletes := slices.Sorted(maps.Keys(d.pendingDeletes))
	if len(page) == 0 && len(deletes) == 0 {
		return
	}
	if !confirmLeave(len(page)+len(deletes), "change(s) to the secrets manager") {
		for _, name := range page {
			restoreStaged(d, name, before)
		}
		d.pendingDeletes = nil
		d.printf("secrets: %d change(s) discarded\n", len(page)+len(deletes))
		return
	}
	for _, name := range page {
		k := d.pending[name]
		if err := secrets.Store(name, k.val); err != nil {
			d.printf("%s: not saved: %v\n", name, err)
			continue
		}
		d.printf("saved %s to the vault (%s)\n", name, secrets.Redact(k.val))
		delete(d.pending, name) // stored: the env it exported stays, Submit has nothing left to do for it
	}
	for _, name := range deletes {
		if err := secrets.Delete(name); err != nil {
			d.printf("%s: not deleted: %v\n", name, err)
			continue
		}
		audit.Log(audit.SecretDelete, name, "")
		d.printf("deleted %s\n", name)
	}
	d.pendingDeletes = nil
	d.changed = true
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
