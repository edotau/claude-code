package vaultcmd

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

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
	if err := agent.SetSecret(name, value); err != nil {
		return helptext.Fail("vault set: %v", err)
	}
	router.ClearSecretAuth(name)
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
	lookup, masterKey := names, slices.Contains(names, "HARNESS_VAULT_KEY")
	if masterKey {
		lookup = names[:slices.Index(names, "HARNESS_VAULT_KEY")]
	}
	values, err := vault.LookupAll(lookup)
	if err != nil {
		return helptext.Fail("vault get: %v", err)
	}
	if masterKey {
		return helptext.Fail("vault get: HARNESS_VAULT_KEY is the master key; use vault get --key (refused on a terminal)")
	}
	if err := printVaultValues(names, values, *export); err != nil {
		return helptext.Fail("vault get: %v", err)
	}
	return 0
}

// printVaultValues prints NAME=value lines, or shell-quoted export lines with export; the first write error is returned.
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
	if _, err := fmt.Println(k.Raw()); err != nil {
		fmt.Fprintln(os.Stderr, "vault get --key:", err)
		return 1
	}
	audit.Log(audit.KeyReveal, k.Kid, "")
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

// cmdVaultImport stores a .env's NAME=value lines; any malformed line aborts before anything is written.
// "-" reads stdin: no confirm prompt can read the data stream, so it needs --yes and refuses a terminal.
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
	pairs, problems := vault.ParseDotenv(r)
	if len(problems) > 0 {
		return helptext.Fail("vault import: %s: nothing written\n  %s", name, strings.Join(problems, "\n  "))
	}
	if len(pairs) == 0 {
		return helptext.Fail("vault import: %s has no NAME=value lines", name)
	}
	if err := configure.ValidateImport(pairs); err != nil {
		return helptext.Fail("vault import: %s: %v", name, err)
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

// openImport opens FILE, or piped stdin for "-" after the terminal and --yes checks; a nil reader carries the exit code.
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

// confirmImport previews each pair (masked) and, on a terminal without --yes, asks before storing; false means nothing written.
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

// cmdVaultExport appends stored secrets (all, or the NAMEs given) to --out (default ./.env): names the file
// already defines are skipped, and the file is left 0600 whether created or appended to.
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
	if err != nil {
		return helptext.Fail("vault %v", err)
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
