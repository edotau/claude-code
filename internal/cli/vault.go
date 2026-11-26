package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/tailscale"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/menu"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaultdoc"
)

// vaultWriteSections are the sections `vault set` and `vault rm` take.
var vaultWriteSections = []string{"secrets", "env", "overlay"}

func vaultSections() []string { return append(slices.Clone(vaultdoc.Sections), "all", "overlays") }

// vaultSpec is the completion node for the vault verb.
func vaultSpec() *helptext.Node {
	flag, boolFlag, formatFlag := helptext.Plain, helptext.BoolFlag, helptext.CompletionFormatFlag
	secretNames := providers.SecretNames
	writeSections := func() []string { return vaultWriteSections }
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"ls": {Args: []func() []string{vaultSections}, Flags: []helptext.Flag{boolFlag("tailscale"), formatFlag()}},
		"set": {Args: []func() []string{writeSections, secretNames}, Flags: []helptext.Flag{boolFlag("tailscale"), boolFlag("api-key"),
			boolFlag("dry-run"), boolFlag("yes"), {Name: "file"}}},
		"get":           {Args: []func() []string{secretNames}, Flags: []helptext.Flag{boolFlag("export"), boolFlag("key"), boolFlag("secret")}},
		"rm":            {Args: []func() []string{writeSections, secretNames}, Flags: []helptext.Flag{boolFlag("dry-run"), boolFlag("yes")}},
		"import":        {Flags: []helptext.Flag{boolFlag("yes")}},
		"export":        {Args: []func() []string{secretNames}, Flags: []helptext.Flag{flag("out")}},
		"rotate-secret": {Flags: []helptext.Flag{boolFlag("force")}},
	}}
}

var vaultPage = helptext.Page{
	Usage: "claude-code vault [<command>] [flags]",
	About: "The encrypted store (state/vault.db): sealed secrets, pins, env, and the\n" +
		"profile and agent overlays. Values print only through get.\n" +
		"Interactive editor: claude-code configure.",
	Commands: [][2]string{
		{"ls", "everything stored (masked); bare vault runs it; ls overlays"},
		{"set", "<secrets|env|overlay> NAME: store; --tailscale; bare: dialog"},
		{"get", "print secrets redacted; --secret for values, --key for master"},
		{"rm", "<secrets|env|overlay> NAME: remove one entry"},
		{"import", "seal every NAME=value line of a .env file"},
		{"export", "append secrets to ./.env (0600); --out for another path"},
		{"rotate-secret", "re-seal every secret under a new master key"},
	},
	Hint: "claude-code vault <command> -h lists that command's arguments and flags.",
}

// vaultVerbs is the vault subcommand table.
var vaultVerbs = map[string]func(args []string) int{
	"ls": vaultList, "set": cmdVaultSet, "get": cmdVaultGet, "rm": cmdVaultRm,
	"import": cmdVaultImport, "export": cmdVaultExport, "rotate-secret": cmdVaultRotateSecret,
}

// vaultRemoved names the form that replaced each folded verb (a hint, not an alias).
var vaultRemoved = map[string]string{
	"delete":  "claude-code vault rm secrets NAME",
	"env":     "claude-code vault set env NAME VALUE, or vault rm env NAME",
	"overlay": "claude-code vault set overlay KEY --file PATH|-, vault rm overlay KEY, or vault ls overlays",
}

// cmdVault dispatches the vault subcommands; bare `vault` lists.
func cmdVault(args []string) int {
	if len(args) == 0 || (strings.HasPrefix(args[0], "-") && !helptext.WantsHelp(args[:1])) {
		return vaultList(args)
	}
	sub, args := args[0], args[1:]
	if isHelpVerb(sub) {
		vaultPage.Fprint(os.Stdout)
		return 0
	}
	if run, ok := vaultVerbs[sub]; ok {
		return run(args)
	}
	if use, gone := vaultRemoved[sub]; gone {
		fmt.Fprintf(os.Stderr, "vault %s is gone; use %s\n", sub, use)
		return 2
	}
	return unknownSub("vault", sub, vaultPage)
}

// vaultUsageErr reports a stray argument to a vault subcommand; a -h there prints the page and exits 0.
func vaultUsageErr(sub string, args []string) int {
	if helptext.WantsHelp(args[:1]) {
		vaultPage.Fprint(os.Stdout)
		return 0
	}
	fmt.Fprintf(os.Stderr, "vault %s: unexpected argument %q\n", sub, args[0])
	vaultPage.Fprint(os.Stderr)
	return 2
}

// cmdVaultSet stores a secret (default section), an env value or an overlay document; --tailscale adds a credential.
func cmdVaultSet(args []string) int {
	fs := helptext.NewFlagSet("vault set", helptext.Page{
		Usage: "claude-code vault set [<secrets|env|overlay> NAME [VALUE]] [flags]",
		About: "secrets NAME: sealed; value from a hidden prompt or stdin (bare: dialog).\n" +
			"env NAME VALUE: plain value injected into launches.\n" +
			"overlay KEY --file PATH|-: validated document. " + vaultOverlaySetAbout,
	})
	ts := fs.Bool("tailscale", false, "add a Tailscale credential (OAuth or API key) instead of NAME")
	apiKey := fs.Bool("api-key", false, "with --tailscale: piped stdin is one TAILSCALE_API_KEY value")
	dryRun := fs.Bool("dry-run", false, "report what would be written; change nothing")
	yes := fs.Bool("yes", false, "env: overwrite an existing value without asking; required off a terminal")
	fs.BoolVar(yes, "y", false, "same as --yes")
	file := fs.String("file", "", "overlay: document to store, a path or - for stdin")
	words, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	section := "secrets" // bare `vault set` (dialog) and --tailscale name no section
	if len(words) > 0 {
		if !slices.Contains(vaultWriteSections, words[0]) {
			fmt.Fprintln(os.Stderr, "vault set NAME is gone; use claude-code vault set secrets NAME")
			return 2
		}
		section, words = words[0], words[1:]
	}
	switch {
	case section == "env" && len(words) == 2:
		return vaultEnvSet(words[0], words[1], *dryRun, *yes)
	case section == "overlay" && len(words) == 1:
		return vaultOverlaySet(words[0], *file, *dryRun)
	case section != "secrets":
		fs.Usage()
		return 2
	}
	return vaultSetSecrets(words, *ts, *apiKey, *dryRun)
}

// vaultSetSecrets is `vault set secrets NAME`, the bare dialog and the --tailscale credentials.
func vaultSetSecrets(names []string, ts, apiKey, dryRun bool) int {
	if ts {
		if len(names) > 0 {
			return vaultUsageErr("set", names)
		}
		if tui.StdinIsTerminal() {
			return vaultSetTailscaleInteractive(dryRun)
		}
		return vaultSetTailscalePiped(apiKey, dryRun)
	}
	if apiKey {
		return helptext.FailUsage("vault set: --api-key needs --tailscale")
	}
	if len(names) == 0 && !dryRun && tui.StdinIsTerminal() {
		return menu.Secrets(menu.Options{In: os.Stdin, Out: os.Stdout, Interactive: true})
	}
	if len(names) != 1 {
		return helptext.FailUsage("vault set: usage: claude-code vault set secrets NAME")
	}
	return vaultSetSecret(names[0], dryRun)
}

// vaultSetTailscaleInteractive asks which credential kind, then reads each value hidden.
func vaultSetTailscaleInteractive(dryRun bool) int {
	fmt.Fprint(os.Stdout, "OAuth client (o) or API key (a)? ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return helptext.Fail("vault set: read credential kind: %v", err)
	}
	return readTailscaleCreds(tailscale.SetNames(strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "a")),
		readSecretValue, dryRun)
}

// vaultSetTailscalePiped reads one line per expected value: --api-key wants one, otherwise id then secret.
func vaultSetTailscalePiped(apiKeyOnly, dryRun bool) int {
	r := bufio.NewReader(os.Stdin)
	readLine := func(string) (string, error) {
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	return readTailscaleCreds(tailscale.SetNames(apiKeyOnly), readLine, dryRun)
}

// readTailscaleCreds reads each named value with read, then validates and stores them.
func readTailscaleCreds(names []string, read func(name string) (string, error), dryRun bool) int {
	pairs := make([]tailscale.Cred, len(names))
	for i, name := range names {
		v, err := read(name)
		if err != nil {
			return helptext.Fail("vault set: %v", err)
		}
		pairs[i] = tailscale.Cred{Name: name, Value: v}
	}
	return vaultStoreTailscale(dryRun, pairs)
}

// vaultStoreTailscale validates every value, then stores all or none (StoreCreds rolls back); dry-run reports.
func vaultStoreTailscale(dryRun bool, creds []tailscale.Cred) int {
	if err := tailscale.CheckCreds(creds); err != nil {
		fmt.Fprintf(os.Stderr, "vault set: %v; nothing written\n", err)
		return 1
	}
	if dryRun {
		for _, c := range creds {
			fmt.Printf("would %s %s (%d chars); nothing written\n", tailscale.DryVerb(c), c.Name, len(c.Value))
		}
		return 0
	}
	if err := tailscale.StoreCreds(creds); err != nil {
		return helptext.Fail("vault set: %v", err)
	}
	for _, c := range creds {
		fmt.Fprintf(os.Stderr, "wrote %s to the vault (%s)\n", c.Name, vault.Redact(c.Value))
	}
	return 0
}

func cmdVaultRotateSecret(args []string) int {
	fs := helptext.NewFlagSet("vault rotate-secret", helptext.Page{
		Usage: "claude-code vault rotate-secret [flags]",
		About: "Re-seal every secret under a new master key; values unchanged.",
	})
	force := fs.Bool("force", false, "rotate even if HARNESS_VAULT_KEY or key_command is set")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return vaultUsageErr("rotate-secret", fs.Args())
	}
	if err := vaultdoc.RotateKey(os.Stdout, *force); err != nil {
		return helptext.Fail("vault rotate-secret: %v", err)
	}
	return 0
}

// vaultList prints the selected sections (default all): secrets masked, then the vault's non-secret config.
func vaultList(args []string) int {
	fs := helptext.NewFlagSet("vault ls", helptext.Page{
		Usage: "claude-code vault ls [<section>...|all|overlays]\nclaude-code vault ls providers [<name>] [--format text|json]\n[--tailscale]",
		About: "Print what the vault stores, secrets masked. Sections: " +
			strings.Join(vaultdoc.Sections, ", ") + "; bare or all prints every one.\n" +
			"providers alone lists the merged profiles with credential state (present or absent, never a value).",
	})
	ts := fs.Bool("tailscale", false, "show Tailscale credential presence, not JSON")
	format := fs.String("format", "", "text or json (default: text on a terminal, json otherwise)")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if slices.Contains(names, "overlays") {
		return vaultListOverlays(len(names) == 1 && !*ts && *format == "")
	}
	if *ts {
		return vaultListTailscale(names, *format)
	}
	f := listFormat(*format)
	if name, ok := providerListName(names); ok {
		return providerList(name, f)
	}
	sel, err := vaultdoc.Select(names)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vault ls:", err)
		return 2
	}
	if err := helptext.CheckFormat(f); err != nil {
		return helptext.FailUsage("vault ls: %v", err)
	}
	if err := vaultdoc.List(os.Stdout, f, sel); err != nil {
		return helptext.Fail("vault ls: %v", err)
	}
	return 0
}

// providerList prints every provider's credential state, or one provider's detail, from the shared vaultdoc rows.
func providerList(name, format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("vault ls providers", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if format == helptext.JSON {
		v, err := vaultdoc.ProviderJSON(reg, name)
		if err != nil {
			return helptext.Fail("%v", err)
		}
		return helptext.WriteJSONOrFail("vault ls providers", v)
	}
	lines, err := vaultdoc.ProviderLines(reg, name)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if err := printLines(lines); err != nil {
		return helptext.Fail("vault ls providers: %v", err)
	}
	return 0
}

// providerListName reports `providers [<name>]`, where a second word that is no section is a provider name.
func providerListName(names []string) (name string, ok bool) {
	if len(names) == 0 || len(names) > 2 || !strings.EqualFold(names[0], "providers") {
		return "", false
	}
	if len(names) == 1 {
		return "", true
	}
	if _, err := vaultdoc.Select(names[1:]); err == nil {
		return "", false
	}
	return names[1], true
}

// vaultListTailscale is `vault ls --tailscale`: credential presence, no sections and no --format.
func vaultListTailscale(names []string, format string) int {
	if len(names) > 0 {
		return vaultUsageErr("ls", names)
	}
	if format != "" {
		return helptext.FailUsage("vault ls: --format does not apply with --tailscale")
	}
	vaultdoc.Tailscale(os.Stdout)
	return 0
}

// vaultListOverlays is `vault ls overlays`, valid only alone and without flags.
func vaultListOverlays(alone bool) int {
	if !alone {
		return helptext.FailUsage("vault ls: overlays takes no other section or flag")
	}
	return vaultOverlayList()
}

// listFormat is format, or text on a terminal and json otherwise.
func listFormat(format string) string {
	if format != "" {
		return format
	}
	if tui.StdoutIsTerminal() {
		return helptext.Text
	}
	return helptext.JSON
}

// vaultSetSecret stores one NAME whose value comes from a hidden prompt or stdin.
func vaultSetSecret(name string, dryRun bool) int {
	value, err := readSecretValue(name)
	if err != nil {
		return helptext.Fail("vault set: %v", err)
	}
	if value == "" {
		return helptext.Fail("vault set: empty value")
	}
	if err := agent.ValidateVaultSecret(name, value); err != nil {
		return helptext.Fail("vault set: %v", err)
	}
	login := providers.LoginSecretProvider(name)
	if login != nil {
		if err := providers.CheckLoginRecord(name, value); err != nil {
			return helptext.Fail("vault set: %v", err)
		}
	}
	if dryRun {
		return vaultSetPreview(name, len(value), login)
	}
	if err := configure.StoreSecret(name, value); err != nil {
		return helptext.Fail("vault set: %v", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s to the vault (%s); `claude-code vault get --secret %s` prints it\n", name, vault.Redact(value), name)
	if login != nil {
		printLoginVerdict(login)
	}
	return 0
}

// vaultSetPreview prints what `vault set` would do for name without writing.
func vaultSetPreview(name string, chars int, login *providers.Provider) int {
	names, err := vault.NamesErr()
	if err != nil {
		return helptext.Fail("vault set: %v", err)
	}
	verb := "add"
	if slices.Contains(names, name) {
		verb = "replace"
	}
	fmt.Printf("would %s %s in the vault (%d chars)%s; nothing written\n", verb, name, chars, verifyNote(login))
	return 0
}

// verifyNote is the dry-run clause for a login secret: it would be verified and sealed once the write happens.
func verifyNote(login *providers.Provider) string {
	if login == nil {
		return ""
	}
	return ", would verify and seal"
}

// readSecretValue prompts with hidden input on a terminal; a pipe still supplies the value on stdin.
func readSecretValue(name string) (string, error) {
	if tui.StdinIsTerminal() {
		v, err := agent.PromptSecret(name)
		return strings.TrimSpace(v), err
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// cmdVaultGet prints NAME=<redacted: N bytes> per name, the value with --secret; --key selects the master key.
func cmdVaultGet(args []string) int {
	fs := helptext.NewFlagSet("vault get", helptext.Page{
		Usage: "claude-code vault get NAME... [flags]",
		About: "Print each secret as NAME=<redacted: N bytes>; --secret prints NAME=value (vault, then env).",
	})
	export := fs.Bool("export", false, "with --secret: print export NAME='value' lines for a shell")
	secret := helptext.SecretFlag(fs)
	key := fs.Bool("key", false, "select the master key (base64); with --secret refused on a terminal")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *key {
		if len(names) > 0 || *export {
			return helptext.FailUsage("vault get: --key takes no NAME and no --export")
		}
		return vaultGetKey(*secret)
	}
	if len(names) == 0 {
		fs.Usage()
		return 2
	}
	if *export && !*secret {
		return helptext.FailUsage("vault get: --export needs --secret")
	}
	if slices.Contains(names, "HARNESS_VAULT_KEY") {
		return helptext.Fail("vault get: HARNESS_VAULT_KEY is the master key; use vault get --key")
	}
	values, err := vault.LookupAll(names)
	if err != nil {
		return helptext.Fail("vault get: %v", err)
	}
	lines := make([]helptext.Secret, len(names))
	for i, name := range names {
		lines[i] = helptext.Secret{Prefix: name + "=", Value: values[i]}
		if *export {
			lines[i] = helptext.Secret{Prefix: "export " + name + "=", Value: textutil.ShellQuote(values[i])}
		}
	}
	return helptext.PrintSecrets("vault get", *secret, lines...)
}

// vaultGetKey prints the master key redacted; with --secret only to a pipe, so it never lands in scrollback.
func vaultGetKey(secret bool) int {
	if secret && tui.StdoutIsTerminal() {
		return helptext.FailUsage("vault get --key --secret: refusing to print the master key to a terminal; pipe it")
	}
	k, _, err := vault.MasterKey()
	if err != nil {
		return helptext.Fail("vault get --key: %v", err)
	}
	if secret {
		audit.Log(audit.KeyReveal, k.Kid, "")
	}
	return helptext.PrintSecrets("vault get --key", secret, helptext.Secret{Value: k.Raw()})
}

// cmdVaultRm removes one NAME from a section: a sealed secret, an env value or an overlay document.
func cmdVaultRm(args []string) int {
	fs := helptext.NewFlagSet("vault rm", helptext.Page{
		Usage: "claude-code vault rm <secrets|env|overlay> [flags] NAME",
		About: "Remove a sealed secret, an env value or an overlay document.",
	})
	dryRun := fs.Bool("dry-run", false, "report what would be removed; write nothing")
	yes := fs.Bool("yes", false, "env, overlay: remove without asking; required off a terminal")
	fs.BoolVar(yes, "y", false, "same as --yes")
	words, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(words) != 2 || !slices.Contains(vaultWriteSections, words[0]) {
		fs.Usage()
		return 2
	}
	name := words[1]
	switch words[0] {
	case "env":
		return vaultEnvUnset(name, *dryRun, *yes)
	case "overlay":
		return vaultOverlayUnset(name, *dryRun, *yes)
	}
	return vaultRmSecret(name, *dryRun)
}

// vaultRmSecret removes one sealed secret; --dry-run only checks it is stored.
func vaultRmSecret(name string, dryRun bool) int {
	if dryRun {
		names, err := vault.NamesErr()
		if err != nil {
			return helptext.Fail("vault rm: %v", err)
		}
		verb := "removed"
		if !slices.Contains(names, name) {
			verb = "left (not stored)"
		}
		fmt.Printf("dry run: %s would be %s; nothing written\n", name, verb)
		return 0
	}
	if err := vault.Remove(name); err != nil {
		return helptext.Fail("vault rm: %v", err)
	}
	fmt.Fprintf(os.Stderr, "deleted %s\n", name)
	return 0
}

// cmdVaultImport stores a .env's valid NAME=value lines, warning per bad one; "-" (stdin) needs --yes, no terminal.
func cmdVaultImport(args []string) int {
	fs := helptext.NewFlagSet("vault import", helptext.Page{
		Usage: "claude-code vault import FILE|- [flags]",
		About: "Store each NAME=value line of a .env (# lines skipped); - reads stdin.",
	})
	yes := fs.Bool("yes", false, "skip the confirm; required for - (stdin)")
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(rest) != 1 {
		fs.Usage()
		return 2
	}
	name := rest[0]
	r, code := openImport(name, *yes)
	if r == nil {
		return code
	}
	defer r.Close()
	pairs, code := importPairs(name, r)
	if pairs == nil {
		return code
	}
	if pairs = loginPairsOnly(pairs); len(pairs) == 0 {
		return helptext.Fail("vault import: %s: nothing written", name)
	}
	if !confirmImport(pairs, *yes) {
		return 1
	}
	err = configure.StoreImport(pairs, func(n string) {
		fmt.Fprintf(os.Stderr, "stored %s in the vault\n", n)
		verifyIfLogin(n)
	})
	if err != nil {
		return helptext.Fail("vault import: %v", err)
	}
	return 0
}

// importPairs parses r and warns about each line or pair it skips; nil pairs carry the exit code (nothing importable).
func importPairs(name string, r io.Reader) ([]vault.Pair, int) {
	pairs, problems := configure.ImportablePairs(vault.ParseDotenv(r))
	switch {
	case len(pairs) == 0 && len(problems) > 0:
		return nil, helptext.Fail("vault import: %s: nothing written\n  unable to parse %s", name,
			strings.Join(problems, "\n  unable to parse "))
	case len(pairs) == 0:
		return nil, helptext.Fail("vault import: %s has no NAME=value lines", name)
	}
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "vault import: warning: unable to parse %s; skipped\n", p)
	}
	return pairs, 0
}

// openImport opens FILE, or piped stdin for "-" after the terminal and --yes checks; nil carries the exit code.
func openImport(name string, yes bool) (io.ReadCloser, int) {
	if name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return nil, helptext.Fail("vault import: %v", err)
		}
		return f, 0
	}
	if tui.StdinIsTerminal() {
		return nil, helptext.Fail("vault import -: refusing, stdin is a terminal (pipe data in)")
	}
	if !yes {
		return nil, helptext.Fail("vault import -: needs --yes (no confirm prompt possible on piped stdin)")
	}
	return io.NopCloser(os.Stdin), 0
}

// confirmImport previews each pair (masked) and, on a terminal without --yes, asks first; false = nothing written.
func confirmImport(pairs []vault.Pair, yes bool) bool {
	for _, p := range pairs {
		fmt.Fprintf(os.Stderr, "  %-28s %s\n", p.Name, vault.Preview(p.Value))
	}
	if !yes && tui.StdinIsTerminal() && !tui.ConfirmKey(fmt.Sprintf("Store %d secret(s)?", len(pairs)), true) {
		fmt.Fprintln(os.Stderr, "vault import: nothing written")
		return false
	}
	return true
}

// cmdVaultExport appends secrets to --out (default ./.env), skipping names it defines; the file ends 0600.
func cmdVaultExport(args []string) int {
	fs := helptext.NewFlagSet("vault export", helptext.Page{
		Usage: "claude-code vault export [NAME...] [flags]",
		About: "Append stored secrets to a .env file (0600); names already there are kept.",
	})
	out := fs.String("out", ".env", "file to append to; vault get prints to stdout instead")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *out == "-" {
		fmt.Fprintln(os.Stderr, "vault export: --out - is gone; use claude-code vault get NAME... to print")
		return 2
	}
	pairs, err := vault.ExportPairs(names)
	if err != nil { // ExportPairs prefixes only its per-name failures with "export: "
		return helptext.Fail("vault export: %s", strings.TrimPrefix(err.Error(), "export: "))
	}
	if len(pairs) == 0 {
		return helptext.Fail("vault export: no stored secrets to export")
	}
	added, skipped, err := vault.AppendDotenv(*out, pairs)
	if err != nil {
		return helptext.Fail("vault export: %v", err)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "kept %s (already in %s)\n", strings.Join(skipped, ", "), *out)
	}
	fmt.Fprintf(os.Stderr, "wrote %d secret(s) to %s (0600, plaintext — keep it out of git)\n", added, *out)
	return 0
}
