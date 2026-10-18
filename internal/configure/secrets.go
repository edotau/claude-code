package configure

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
)

// envName is a valid secret name: an env var identifier.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// trackedSecret is one row of the secrets dialog: a name the harness knows of and where it resolves today.
type trackedSecret struct {
	name   string
	source string // keyring|file|env|"" (missing)
	users  string // providers that read it, or a short role
}

// trackedSecrets is what the harness reads (each enabled provider's auth.env, the setup-token) plus every secret
// added through the dialog or `secrets set` (the names index) and anything still in env.d/secrets.env.
func trackedSecrets(reg *providers.Registry) []trackedSecret {
	users := map[string][]string{
		providers.OAuthTokenEnv: {"claude -p (setup-token)"},
	}
	for _, n := range reg.Names() {
		if env := reg.Providers[n].Auth.Env; env != "" {
			users[env] = append(users[env], n)
		}
	}
	custom := secrets.Names()
	for env := range secrets.ReadAll() {
		custom = append(custom, env)
	}
	for _, env := range custom {
		if _, ok := users[env]; !ok {
			users[env] = []string{"custom"}
		}
	}
	out := make([]trackedSecret, 0, len(users))
	for name, u := range users {
		_, src, _ := secrets.Get(name)
		out = append(out, trackedSecret{name: name, source: src, users: strings.Join(u, ", ")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// sourceLabel is the status token a row shows; "file" is called out as plaintext since that is the point.
func sourceLabel(src string) string {
	switch src {
	case "":
		return "missing"
	case "file":
		return "file:plaintext"
	}
	return src
}

// Secrets is the guided `secrets set`: pick a tracked secret (or a new custom one), choose how to satisfy it,
// and store it in the OS keyring (env.d/secrets.env when forceFile or no keyring). Loops until Esc or done.
func Secrets(o Options, forceFile bool) int {
	reg, err := providers.Load()
	if err != nil {
		fmt.Fprintf(o.writer(), "secrets: %v\n", err)
		return 1
	}
	o.In = o.reader()
	d := &dialog{o: o, reg: reg, br: bufio.NewReader(o.In), out: o.writer(), color: ansi.Enabled(o.writer())}
	store := "the OS keyring"
	if forceFile || !keyring.Available() {
		store = secrets.EnvFile()
	}
	for {
		list := trackedSecrets(reg)
		rows := make([][2]string, 0, len(list)+2)
		for _, s := range list {
			rows = append(rows, [2]string{s.name, fmt.Sprintf("%-15s %s", sourceLabel(s.source), s.users)})
		}
		rows = append(rows, [2]string{"+ New", "custom NAME, then paste"}, [2]string{"Done", ""})
		idx, err := pick(d, "secrets — pick one to set (stores in "+store+")", rows)
		if err != nil {
			fmt.Fprintf(d.out, "secrets: %v\n", err)
			return 1
		}
		if idx < 0 || idx == len(rows)-1 {
			return 0
		}
		if idx == len(list) {
			addCustom(d, forceFile)
			continue
		}
		editSecret(d, list[idx], forceFile)
	}
}

// pick is tui.Pick on the controlling terminal, a numbered prompt otherwise (tests, pipes).
func pick(d *dialog, title string, rows [][2]string) (int, error) {
	if isStdinTTY() && d.o.In == os.Stdin {
		return tui.PickBack(title, rows, 0)
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

// editSecret offers every action for one tracked secret: move a plaintext copy into the keyring, mint, log in
// via the browser/console page and paste, or delete.
func editSecret(d *dialog, s trackedSecret, forceFile bool) {
	type action struct {
		name, desc string
		run        func() bool
	}
	var acts []action
	if s.source == "file" && !forceFile && keyring.Available() {
		acts = append(acts, action{"Move to keyring", "drops the plaintext line", func() bool {
			v, _, _ := secrets.Get(s.name)
			return storeValue(d, s.name, v, false)
		}})
	}
	kt := keyTarget{label: s.name, envVar: s.name, fixCmd: "claude-code secrets set " + s.name}
	if s.name == providers.OAuthTokenEnv {
		kt.mintArgv = []string{"claude", "setup-token"}
	}
	for _, o := range authOptions(kt) {
		acts = append(acts, action{o.action, o.desc, func() bool { return runAuthOption(d, kt, o, forceFile) }})
	}
	if s.source == "keyring" || s.source == "file" {
		acts = append(acts, action{"Delete", "keyring + secrets.env", func() bool {
			if !confirm(d, "Delete "+s.name+"?") {
				return false
			}
			if err := secrets.Delete(s.name); err != nil {
				d.printf("%s: %v\n", s.name, err)
				return false
			}
			d.printf("deleted %s\n", s.name)
			return true
		}})
	}
	rows := make([][2]string, 0, len(acts))
	for _, a := range acts {
		rows = append(rows, [2]string{a.name, a.desc})
	}
	idx, err := pick(d, s.name+" ("+sourceLabel(s.source)+")", rows)
	if err != nil || idx < 0 || idx >= len(acts) {
		return
	}
	acts[idx].run()
}

// addCustom reads a NAME (visible) and its value (hidden); a pasted NAME=value or `export NAME="value"` line is
// parsed, so either half may carry the name.
func addCustom(d *dialog, forceFile bool) {
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
		storeValue(d, name, val, forceFile)
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

// storeValue writes one secret via secrets.Store and reports where it landed, redacted.
func storeValue(d *dialog, name, val string, forceFile bool) bool {
	plane, err := secrets.Store(name, val, forceFile)
	if err != nil {
		d.printf("%s: %v\n", name, err)
		return false
	}
	d.printf("wrote %s to %s (%s)\n", name, plane, secrets.Redact(val))
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
