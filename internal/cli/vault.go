package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/config"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tailscale"
	"github.com/edotau/claude-code/internal/tui"
)

const vaultUsage = `usage: claude-code vault <subcommand>
show        print the vault's pins, env, overlays and key_command as JSON;
            secrets never appear (use claude-code secrets) [--tailscale]
set         only --tailscale: add a Tailscale credential [--api-key]
            [--dry-run]; bare "set" points at claude-code secrets set
edit        $VISUAL/$EDITOR on that JSON; saved in one transaction,
            refused if the vault changed meanwhile
migrate     import env.d into the vault; sources become *.bak [--dry-run]
finalize    delete the *.bak files and imported keyring copies once
            verified [--discard NAME]...
rollback    restore the *.bak files and set the vault aside
rotate-key  re-seal every secret under a new master key; the old key
            stays in the keyring`

// vaultVerbs is the vault subcommand table.
var vaultVerbs = map[string]func(args []string) int{
	"show": configShow, "set": cmdVaultSet, "edit": configEdit, "migrate": configMigrate,
	"finalize": configFinalize, "rollback": configRollback, "rotate-key": configRotateKey,
}

// cmdVault reads and edits the vault's non-secret config through one JSON document.
func cmdVault(args []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "", "-h", "--help":
		helptext.Fprint(os.Stdout, vaultUsage+"\n")
		return 0
	}
	if run, ok := vaultVerbs[sub]; ok {
		return run(args)
	}
	fmt.Fprintf(os.Stderr, "vault: unknown subcommand %q\n", sub)
	helptext.Fprint(os.Stderr, vaultUsage+"\n")
	return 2
}

// vaultUsageErr reports a stray argument to a vault subcommand.
func vaultUsageErr(sub string, args []string) int {
	fmt.Fprintf(os.Stderr, "vault %s: unexpected argument %q\n", sub, args[0])
	helptext.Fprint(os.Stderr, vaultUsage+"\n")
	return 2
}

func configShow(args []string) int {
	fs := flag.NewFlagSet("vault show", flag.ContinueOnError)
	ts := fs.Bool("tailscale", false, "show Tailscale credential presence, not JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return vaultUsageErr("show", fs.Args())
	}
	if *ts {
		return vaultShowTailscale()
	}
	out, err := config.Export()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vault show:", err)
		return 1
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fmt.Fprintln(os.Stderr, "vault show:", err)
		return 1
	}
	return 0
}

// vaultShowTailscale prints presence (never a value) per credential name, then the api verdict.
func vaultShowTailscale() int {
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

// cmdVaultSet adds a Tailscale credential; no other use is supported (use claude-code secrets set).
func cmdVaultSet(args []string) int {
	fs := flag.NewFlagSet("vault set", flag.ContinueOnError)
	ts := fs.Bool("tailscale", false, "add a Tailscale credential (OAuth or API key)")
	apiKey := fs.Bool("api-key", false, "piped stdin is one TAILSCALE_API_KEY value")
	dryRun := fs.Bool("dry-run", false, "report what would be written; change nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return vaultUsageErr("set", fs.Args())
	}
	if !*ts {
		fmt.Fprintln(os.Stderr, "vault set: only --tailscale; use claude-code secrets set NAME")
		return 2
	}
	if tui.StdinIsTerminal() {
		return vaultSetTailscaleInteractive(*dryRun)
	}
	return vaultSetTailscalePiped(*apiKey, *dryRun)
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
	if len(args) > 0 {
		return vaultUsageErr("edit", args)
	}
	if err := config.Edit(os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "vault edit:", err)
		return 1
	}
	return 0
}

func configMigrate(args []string) int {
	fs := flag.NewFlagSet("vault migrate", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "parse and report; change nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return vaultUsageErr("migrate", fs.Args())
	}
	return configRun("migrate", config.Migrate(os.Stdout, *dry))
}

// discardFlag collects repeatable --discard NAME acknowledgements.
type discardFlag []string

func (d *discardFlag) String() string     { return strings.Join(*d, ",") }
func (d *discardFlag) Set(s string) error { *d = append(*d, s); return nil }

func configFinalize(args []string) int {
	fs := flag.NewFlagSet("vault finalize", flag.ContinueOnError)
	var discard discardFlag
	fs.Var(&discard, "discard", "drop NAME's differing keyring/backup value (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return vaultUsageErr("finalize", fs.Args())
	}
	return configRun("finalize", config.Finalize(os.Stdout, discard))
}

func configRollback(args []string) int {
	if len(args) > 0 {
		return vaultUsageErr("rollback", args)
	}
	return configRun("rollback", config.Rollback(os.Stdout))
}

func configRotateKey(args []string) int {
	if len(args) > 0 {
		return vaultUsageErr("rotate-key", args)
	}
	return configRun("rotate-key", config.RotateKey(os.Stdout))
}

// configRun maps a subcommand's error to its exit code.
func configRun(sub string, err error) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "vault %s: %v\n", sub, err)
		return 1
	}
	return 0
}
