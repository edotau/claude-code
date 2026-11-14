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
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/menu"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaultdoc"
)

func vaultSections() []string { return append(slices.Clone(vaultdoc.Sections), "all") }

// vaultSpec is the completion node for the vault verb.
func vaultSpec() *helptext.Node {
	flag, boolFlag, formatFlag := helptext.Plain, helptext.BoolFlag, helptext.CompletionFormatFlag
	secretNames := providers.SecretNames
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"ls": {Args: []func() []string{vaultSections}, Flags: []helptext.Flag{boolFlag("tailscale"), formatFlag()}},
		"set": {Args: []func() []string{secretNames}, Flags: []helptext.Flag{boolFlag("tailscale"), boolFlag("api-key"),
			boolFlag("dry-run")}},
		"get":           {Args: []func() []string{secretNames}, Flags: []helptext.Flag{boolFlag("export"), boolFlag("key")}},
		"delete":        {Args: []func() []string{secretNames}},
		"env":           {Sub: map[string]*helptext.Node{"set": {Flags: []helptext.Flag{boolFlag("dry-run")}}, "unset": {Flags: []helptext.Flag{boolFlag("dry-run")}}}},
		"import":        {Flags: []helptext.Flag{boolFlag("yes")}},
		"export":        {Args: []func() []string{secretNames}, Flags: []helptext.Flag{flag("out")}},
		"rotate-secret": {Flags: []helptext.Flag{boolFlag("force")}},
	}}
}

var vaultPage = helptext.Page{
	Usage: "claude-code vault [<command>] [flags]",
	About: "The encrypted store (state/vault.db): sealed secrets, pins, env, and the\n" +
		"profile and agent overlays. Values print only through get.",
	Commands: [][2]string{
		{"ls", "everything stored, secrets first (masked); bare vault runs it"},
		{"set", "store a secret; bare opens a guided dialog; --tailscale"},
		{"get", "print secret values; --export for shell, --key for master"},
		{"delete", "remove a secret"},
		{"env", "set or unset one env value (not sealed); ls env lists"},
		{"import", "seal every NAME=value line of a .env file"},
		{"export", "append secrets to ./.env (0600); --out for another path"},
		{"rotate-secret", "re-seal every secret under a new master key"},
	},
	Hint: "claude-code vault <command> -h lists that command's arguments and flags.",
}

// vaultVerbs is the vault subcommand table.
var vaultVerbs = map[string]func(args []string) int{
	"ls": vaultList, "set": cmdVaultSet, "get": cmdVaultGet, "delete": cmdVaultDelete,
	"env": cmdVaultEnv, "import": cmdVaultImport, "export": cmdVaultExport, "rotate-secret": cmdVaultRotateSecret,
}

// cmdVault dispatches the vault subcommands; bare `vault` lists.
func cmdVault(args []string) int {
	if len(args) == 0 || (strings.HasPrefix(args[0], "-") && !helptext.WantsHelp(args[:1])) {
		return vaultList(args) // bare `vault` (or a leading flag) is `vault ls`
	}
	sub, args := args[0], args[1:]
	if isHelpVerb(sub) {
		vaultPage.Fprint(os.Stdout)
		return 0
	}
	if run, ok := vaultVerbs[sub]; ok {
		return run(args)
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

// cmdVaultSet stores a secret by NAME (or via the guided dialog); --tailscale adds a Tailscale credential.
func cmdVaultSet(args []string) int {
	fs := helptext.NewFlagSet("vault set", helptext.Page{
		Usage: "claude-code vault set [NAME] [flags]",
		About: "Store a secret sealed in the vault; no NAME on a terminal opens the guided dialog.",
	})
	ts := fs.Bool("tailscale", false, "add a Tailscale credential (OAuth or API key) instead of NAME")
	apiKey := fs.Bool("api-key", false, "with --tailscale: piped stdin is one TAILSCALE_API_KEY value")
	dryRun := fs.Bool("dry-run", false, "report what would be written; change nothing")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *ts {
		if len(names) > 0 {
			return vaultUsageErr("set", names)
		}
		if tui.StdinIsTerminal() {
			return vaultSetTailscaleInteractive(*dryRun)
		}
		return vaultSetTailscalePiped(*apiKey, *dryRun)
	}
	if *apiKey {
		return helptext.FailUsage("vault set: --api-key needs --tailscale")
	}
	if len(names) == 0 && !*dryRun && tui.StdinIsTerminal() {
		return menu.Secrets(menu.Options{In: os.Stdin, Out: os.Stdout, Interactive: true})
	}
	if len(names) != 1 {
		fs.Usage()
		return 2
	}
	return vaultSetSecret(names[0], *dryRun)
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
		Usage: "claude-code vault ls [<section>...|all]\n[--format text|json] [--tailscale]",
		About: "Print what the vault stores, secrets masked. Sections: " +
			strings.Join(vaultdoc.Sections, ", ") + "; bare or all prints every one.",
	})
	ts := fs.Bool("tailscale", false, "show Tailscale credential presence, not JSON")
	format := fs.String("format", "", "text or json (default: text on a terminal, json otherwise)")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *ts {
		if len(names) > 0 {
			return vaultUsageErr("ls", names)
		}
		if *format != "" {
			return helptext.FailUsage("vault ls: --format does not apply with --tailscale")
		}
		vaultdoc.Tailscale(os.Stdout)
		return 0
	}
	sel, err := vaultdoc.Select(names)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vault ls:", err)
		return 2
	}
	f := listFormat(*format)
	if err := helptext.CheckFormat(f); err != nil {
		return helptext.FailUsage("vault ls: %v", err)
	}
	if err := vaultdoc.List(os.Stdout, f, sel); err != nil {
		return helptext.Fail("vault ls: %v", err)
	}
	return 0
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
	if dryRun {
		names, err := vault.NamesErr()
		if err != nil {
			return helptext.Fail("vault set: %v", err)
		}
		verb := "add"
		if slices.Contains(names, name) {
			verb = "replace"
		}
		fmt.Printf("would %s %s in the vault (%d chars); nothing written\n", verb, name, len(value))
		return 0
	}
	if err := configure.StoreSecret(name, value); err != nil {
		return helptext.Fail("vault set: %v", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s to the vault (%s); `claude-code vault get %s` prints it\n", name, vault.Redact(value), name)
	return 0
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

// cmdVaultGet prints NAME=value per name (vault -> env), or the master key with --key (pipes only).
func cmdVaultGet(args []string) int {
	fs := helptext.NewFlagSet("vault get", helptext.Page{
		Usage: "claude-code vault get NAME... [flags]",
		About: "Print each secret as NAME=value (vault first, then the process env).",
	})
	export := fs.Bool("export", false, "print export NAME='value' lines for a shell")
	key := fs.Bool("key", false, "print the master key (base64) instead; refused on a terminal")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *key {
		if len(names) > 0 || *export {
			fmt.Fprintln(os.Stderr, "vault get: --key takes no NAME and no --export")
			return 2
		}
		return vaultGetKey()
	}
	if len(names) == 0 {
		fs.Usage()
		return 2
	}
	if slices.Contains(names, "HARNESS_VAULT_KEY") {
		return helptext.Fail("vault get: HARNESS_VAULT_KEY is the master key; use vault get --key (refused on a terminal)")
	}
	values, err := vault.LookupAll(names)
	if err != nil {
		return helptext.Fail("vault get: %v", err)
	}
	if err := printVaultValues(names, values, *export); err != nil {
		return helptext.Fail("vault get: %v", err)
	}
	return 0
}

// printVaultValues prints NAME=value lines, or shell-quoted export lines; the first write error is returned.
func printVaultValues(names, values []string, export bool) error {
	for i, name := range names {
		var err error
		if export {
			_, err = fmt.Printf("export %s=%s\n", name, textutil.ShellQuote(values[i]))
		} else {
			_, err = fmt.Printf("%s=%s\n", name, values[i])
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// vaultGetKey prints the master key only to a pipe, never a terminal, so it never lands in scrollback.
func vaultGetKey() int {
	if tui.StdoutIsTerminal() {
		fmt.Fprintln(os.Stderr, "vault get --key: refusing to print the master key to a terminal; pipe it")
		return 2
	}
	k, _, err := vault.MasterKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vault get --key:", err)
		return 1
	}
	audit.Log(audit.KeyReveal, k.Kid, "")
	if _, err := fmt.Println(k.Raw()); err != nil {
		fmt.Fprintln(os.Stderr, "vault get --key:", err)
		return 1
	}
	return 0
}

// cmdVaultDelete removes NAME from the vault.
func cmdVaultDelete(args []string) int {
	fs := helptext.NewFlagSet("vault delete", helptext.Page{
		Usage: "claude-code vault delete NAME",
		About: "Remove a secret from the vault.",
	})
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(names) != 1 {
		fs.Usage()
		return 2
	}
	name := names[0]
	if err := vault.Remove(name); err != nil {
		return helptext.Fail("vault delete: %v", err)
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
	if !confirmImport(pairs, *yes) {
		return 1
	}
	err = configure.StoreImport(pairs, func(n string) { fmt.Fprintf(os.Stderr, "stored %s in the vault\n", n) })
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
