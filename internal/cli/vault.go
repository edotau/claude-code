package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vault"
)

var vaultPage = helptext.Page{
	Usage: "claude-code vault <command> [flags]",
	About: "The encrypted store (state/vault.db): sealed secrets, pins, env, and the\n" +
		"profile and agent overlays. Values print only through get.",
	Commands: [][2]string{
		{"list", "everything stored, secrets first (masked); --format json"},
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
	"list": vaultList, "set": cmdVaultSet, "get": cmdVaultGet, "delete": cmdVaultDelete,
	"import": cmdVaultImport, "export": cmdVaultExport, "edit": configEdit, "rotate-secret": configRotateKey,
}

// cmdVault reads and edits the vault's non-secret config through one JSON document.
func cmdVault(args []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "", "-h", "--help", "help":
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

// vaultListTailscale prints presence (never a value) per credential name, then the api verdict.
func vaultListTailscale() int {
	cs := tailscale.Credentials()
	for _, c := range cs {
		if c.Present {
			fmt.Printf("%s  set (%s)\n", c.Name, c.Source)
		} else {
			fmt.Printf("%s  missing\n", c.Name)
		}
	}
	fmt.Println("api:", tailscaleAPIVerdict(cs))
	return 0
}

// tailscaleAPIVerdict names which admin credential, if any, Credentials (in its fixed order) resolves to.
func tailscaleAPIVerdict(cs []tailscale.CredState) string {
	id, sec, api := cs[0].Present, cs[1].Present, cs[2].Present
	switch {
	case id && sec:
		return "oauth"
	case api: // NewClient falls back to the API key when the OAuth pair is half-set
		return "api-key"
	case id != sec:
		return "incomplete (need both …_ID and _SECRET)"
	default:
		return "none"
	}
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
	names, err := outfmt.Parse(fs, args)
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
		return configure.Secrets(configure.Options{In: os.Stdin, Out: os.Stdout, Interactive: true})
	}
	if len(names) != 1 {
		fs.Usage()
		return 2
	}
	return vaultSetSecret(names[0], *dryRun)
}

// vaultPair is one credential name/value pair waiting to be reported and stored together.
type vaultPair struct{ Name, Value string }

// vaultSetTailscaleInteractive asks which credential kind, then reads each value hidden.
func vaultSetTailscaleInteractive(dryRun bool) int {
	fmt.Fprint(os.Stdout, "OAuth client (o) or API key (a)? ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	names := tailscaleSetNames(strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "a"))
	pairs := make([]vaultPair, len(names))
	for i, name := range names {
		v, err := readSecretValue(name)
		if err != nil {
			return fail("vault set: %v", err)
		}
		pairs[i] = vaultPair{Name: name, Value: v}
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
	names := tailscaleSetNames(apiKeyOnly)
	pairs := make([]vaultPair, len(names))
	for i, name := range names {
		v, err := readLine()
		if err != nil {
			return fail("vault set: %v", err)
		}
		pairs[i] = vaultPair{Name: name, Value: v}
	}
	return vaultStoreTailscale(dryRun, pairs)
}

// tailscaleSetNames is what `vault set --tailscale` writes: the API key alone, or the OAuth id then secret.
func tailscaleSetNames(apiKey bool) []string {
	if apiKey {
		return []string{"TAILSCALE_API_KEY"}
	}
	return []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET"}
}

// vaultStoreTailscale validates every value before writing any of them, then stores (or dry-run reports) each.
func vaultStoreTailscale(dryRun bool, pairs []vaultPair) int {
	for _, p := range pairs {
		if p.Value == "" {
			fmt.Fprintf(os.Stderr, "vault set: empty value for %s; nothing written\n", p.Name)
			return 1
		}
		if strings.ContainsAny(p.Value, "\r\n") { // secrets.Store rejects these; refuse before a half-written pair
			fmt.Fprintf(os.Stderr, "vault set: %s spans lines; nothing written\n", p.Name)
			return 1
		}
	}
	for _, p := range pairs {
		if dryRun {
			verb := "add"
			if slices.Contains(secrets.Names(), p.Name) {
				verb = "replace"
			}
			fmt.Printf("would %s %s (%d chars); nothing written\n", verb, p.Name, len(p.Value))
			continue
		}
		if err := secrets.Store(p.Name, p.Value); err != nil {
			return fail("vault set: %v", err)
		}
		fmt.Fprintf(os.Stderr, "wrote %s to the vault (%s)\n", p.Name, secrets.Redact(p.Value))
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
	if err := configure.Edit(os.Stdout, os.Stderr); err != nil {
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
	return configRun("rotate-secret", configure.RotateKey(os.Stdout, opts...))
}

// configRun maps a subcommand's error to its exit code.
func configRun(sub string, err error) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "vault %s: %v\n", sub, err)
		return 1
	}
	return 0
}
