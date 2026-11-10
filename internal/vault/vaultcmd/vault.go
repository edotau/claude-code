package vaultcmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/menu"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

var vaultPage = helptext.Page{
	Usage: "claude-code vault [<command>] [flags]",
	About: "The encrypted store (state/vault.db): sealed secrets, pins, env, and the\n" +
		"profile and agent overlays. Values print only through get.",
	Commands: [][2]string{
		{"ls", "everything stored, secrets first (masked); bare vault runs it"},
		{"set", "store a secret; bare opens a guided dialog; --tailscale"},
		{"get", "print secret values; --export for shell, --key for master"},
		{"delete", "remove a secret"},
		{"import", "seal every NAME=value line of a .env file"},
		{"export", "write secrets to ./.env (0600); --out for another path"},
		{"edit", "pins, env and overlays in $EDITOR, one transaction"},
		{"rotate-secret", "re-seal every secret under a new master key"},
	},
	Hint: "claude-code vault <command> -h lists that command's arguments and flags.",
}

// vaultVerbs is the vault subcommand table.
var vaultVerbs = map[string]func(args []string) int{
	"ls": vaultList, "set": cmdVaultSet, "get": cmdVaultGet, "delete": cmdVaultDelete,
	"import": cmdVaultImport, "export": cmdVaultExport, "edit": configEdit, "rotate-secret": configRotateKey,
}

// Run reads and edits the vault's non-secret config through one JSON document.
func Run(args []string) int {
	if len(args) == 0 || (strings.HasPrefix(args[0], "-") && !helptext.WantsHelp(args[:1])) {
		return vaultList(args) // bare `vault` (or a leading flag) is `vault ls`
	}
	sub, args := args[0], args[1:]
	if sub == "-h" || sub == "--help" || sub == "help" {
		vaultPage.Fprint(os.Stdout)
		return 0
	}
	if run, ok := vaultVerbs[sub]; ok {
		return run(args)
	}
	fmt.Fprintf(os.Stderr, "vault: unknown subcommand %q\n", sub)
	vaultPage.Fprint(os.Stderr)
	return 2
}

// vaultUsageErr reports a stray argument to a vault subcommand; a -h there prints the page and exits 0.
func vaultUsageErr(sub string, args []string) int {
	if args[0] == "-h" || args[0] == "--help" {
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
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	names := tailscale.SetNames(strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "a"))
	pairs := make([]tailscale.Cred, len(names))
	for i, name := range names {
		v, err := readSecretValue(name)
		if err != nil {
			return helptext.Fail("vault set: %v", err)
		}
		pairs[i] = tailscale.Cred{Name: name, Value: v}
	}
	return vaultStoreTailscale(dryRun, pairs)
}

// vaultSetTailscalePiped reads one line per expected value: --api-key wants one, otherwise id then secret.
func vaultSetTailscalePiped(apiKeyOnly, dryRun bool) int {
	r := bufio.NewReader(os.Stdin)
	readLine := func() (string, error) {
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	names := tailscale.SetNames(apiKeyOnly)
	pairs := make([]tailscale.Cred, len(names))
	for i, name := range names {
		v, err := readLine()
		if err != nil {
			return helptext.Fail("vault set: %v", err)
		}
		pairs[i] = tailscale.Cred{Name: name, Value: v}
	}
	return vaultStoreTailscale(dryRun, pairs)
}

// vaultStoreTailscale validates every value, then stores them all or none (StoreCreds rolls back); dry-run only reports.
func vaultStoreTailscale(dryRun bool, creds []tailscale.Cred) int {
	if err := tailscale.CheckCreds(creds); err != nil {
		fmt.Fprintf(os.Stderr, "vault set: %v; nothing written\n", err)
		return 1
	}
	if dryRun {
		for _, c := range creds {
			verb, _ := tailscale.StoreCred(c, true) // dry-run never fails
			fmt.Printf("would %s %s (%d chars); nothing written\n", verb, c.Name, len(c.Value))
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

func configEdit(args []string) int {
	fs := helptext.NewFlagSet("vault edit", helptext.Page{
		Usage: "claude-code vault edit",
		About: "Edit pins, env and overlays in $EDITOR, as one transaction.",
	})
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return vaultUsageErr("edit", fs.Args())
	}
	if err := Edit(os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "vault edit:", err)
		return 1
	}
	return 0
}

func configRotateKey(args []string) int {
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
	var opts []vault.RotateKeyOption
	if *force {
		opts = append(opts, vault.ForceRotate())
	}
	return configRun("rotate-secret", rotateKey(os.Stdout, opts...))
}

// configRun maps a subcommand's error to its exit code.
func configRun(sub string, err error) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "vault %s: %v\n", sub, err)
		return 1
	}
	return 0
}
