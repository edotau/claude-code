package cli

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tailscale"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

const secretsUsage = `usage: claude-code secrets <op> [flags]
  set [<NAME>]            store a secret (sealed in the vault)
                          NAME: prompts for the value; no NAME: guided dialog
  get <NAME> [--export]   print a secret (vault -> env)
  delete <NAME>           remove a secret from the vault
  import <file>|-         store each NAME=value line of a .env (# lines skipped)
                          -: read stdin (needs --yes; refused on a terminal)
  export [NAME...]        append stored secrets to ./.env, or --out f (0600)
                          --out -: stream NAME=value lines to stdout instead
  list [NAME...]          stored secrets: name, vault, masked value
  status                  credential source per provider (never the value)
  scan [paths...]         find plaintext secrets in config files and the vault
  git [--since N]         find secret-shaped additions in git history
  doctor                  vault perms, master key, key names, keyring tool
  all                     scan + git + doctor
`

// cmdSecrets dispatches the `secrets` verb.
func cmdSecrets(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		helptext.Fprint(os.Stderr, secretsUsage)
		return 2
	}
	op, rest := args[0], args[1:]
	switch op {
	case "set":
		return cmdSecretsSet(rest)
	case "get":
		return cmdSecretsGet(rest)
	case "delete":
		return cmdSecretsDelete(rest)
	case "import":
		return cmdSecretsImport(rest)
	case "export":
		return cmdSecretsExport(rest)
	case "list":
		return cmdSecretsList(rest)
	case "status":
		return cmdSecretsStatus(rest)
	case "scan":
		return cmdSecretsScan(rest)
	case "git":
		return cmdSecretsGit(rest)
	case "doctor":
		return cmdSecretsDoctor(rest)
	case "all":
		return cmdSecretsAll(rest)
	}
	helptext.Fprint(os.Stderr, secretsUsage)
	return fail("secrets: unknown op %q", op)
}

// extractBool pulls a `--name` boolean flag out of args wherever it appears (NAME --flag and
// --flag NAME must both work — the stdlib `flag` package only accepts flags before positionals).
func extractBool(args []string, name string) (bool, []string) {
	flag := "--" + name
	found := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == flag {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return found, rest
}

// extractValue pulls a `--name V` (or `--name=V`) string flag out of args wherever it appears.
func extractValue(args []string, name string) (string, []string, error) {
	flag, val := "--"+name, ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == flag:
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s needs a value", flag)
			}
			val, i = args[i+1], i+1
		case strings.HasPrefix(a, flag+"="):
			val = strings.TrimPrefix(a, flag+"=")
		default:
			rest = append(rest, a)
		}
	}
	return val, rest, nil
}

// extractInt pulls a `--name N` (or `--name=N`) int flag out of args wherever it appears.
func extractInt(args []string, name string, def int) (int, []string, error) {
	flag := "--" + name
	val := def
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == flag:
			if i+1 >= len(args) {
				return 0, nil, fmt.Errorf("%s needs a value", flag)
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return 0, nil, fmt.Errorf("%s: %v", flag, err)
			}
			val, i = n, i+1
		case strings.HasPrefix(a, flag+"="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, flag+"="))
			if err != nil {
				return 0, nil, fmt.Errorf("%s: %v", flag, err)
			}
			val = n
		default:
			rest = append(rest, a)
		}
	}
	return val, rest, nil
}

func cmdSecretsSet(args []string) int {
	dryRun, rest := extractBool(args, "dry-run")
	if slices.Contains(rest, "--help") || slices.Contains(rest, "-h") {
		helptext.Fprint(os.Stderr, secretsUsage)
		return 2
	}
	if len(rest) == 0 && !dryRun && tui.StdinIsTerminal() {
		return configure.Secrets(configure.Options{In: os.Stdin, Out: os.Stdout, Interactive: true})
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets set [<NAME>] [--dry-run]  (no NAME = dialog)")
		return 2
	}
	name := rest[0]
	value, err := readSecretValue(name)
	if err != nil {
		return fail("secrets set: %v", err)
	}
	if value == "" {
		return fail("secrets set: empty value")
	}
	if name == vendors.CopilotTokenEnv && !vendors.ValidCopilotToken(value) {
		return fail("secrets set: %s not a GitHub token (want gho_/ghp_/ghu_/github_pat_, >=20 chars)", name)
	}
	if err := tailscale.ValidCredential(name, value); err != nil {
		return fail("secrets set: %v", err)
	}
	if dryRun {
		verb := "add"
		if slices.Contains(secrets.Names(), name) {
			verb = "replace"
		}
		fmt.Printf("would %s %s in the vault (%d chars); nothing written\n", verb, name, len(value))
		return 0
	}
	if err := secrets.Store(name, value); err != nil {
		return fail("secrets set: %v", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s to the vault (%s); `claude-code secrets get %s` prints it\n", name, secrets.Redact(value), name)
	return 0
}

// cmdSecretsDelete removes NAME from the vault.
func cmdSecretsDelete(args []string) int {
	if len(args) != 1 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets delete <NAME>")
		return 2
	}
	name := args[0]
	if !slices.Contains(secrets.Names(), name) {
		return fail("secrets delete: %s not set", name)
	}
	if err := secrets.Delete(name); err != nil {
		return fail("secrets delete: %v", err)
	}
	audit.Log(audit.SecretDelete, name, "")
	fmt.Fprintf(os.Stderr, "deleted %s\n", name)
	return 0
}

// readSecretValue prompts with hidden input on a terminal (a bare `secrets set NAME` used to wait on stdin
// with no prompt at all); a pipe still supplies the value on stdin.
func readSecretValue(name string) (string, error) {
	if tui.StdinIsTerminal() {
		v, err := tui.Secret("Paste the value for "+name+" (input hidden, Enter when done): ", secrets.Preview)
		return strings.TrimSpace(v), err
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

func cmdSecretsGet(args []string) int {
	export, rest := extractBool(args, "export")
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets get <NAME> [--export]")
		return 2
	}
	name := rest[0]
	value, _, err := secrets.Lookup(name)
	if err != nil {
		return fail("secrets get: %s: %v", name, err)
	}
	if value == "" {
		return fail("secrets get: %s not set (checked the vault and the process env)", name)
	}
	if export {
		fmt.Printf("export %s=%s\n", name, shellQuote(value))
		return 0
	}
	fmt.Println(value)
	return 0
}

// shellQuote wraps v in single quotes for an `export` line, escaping any embedded single quote.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'"'"'`) + "'"
}

func cmdSecretsStatus([]string) int {
	reg, err := providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	secrets.Status(os.Stdout, reg)
	return 0
}

func cmdSecretsScan(args []string) int {
	s := &secrets.Scanner{Out: os.Stdout}
	return s.ScanConfigs(args)
}

func cmdSecretsGit(args []string) int {
	since, rest, err := extractInt(args, "since", 0)
	if err != nil || len(rest) != 0 {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets git [--since N]")
		return 2
	}
	dir, err := os.Getwd()
	if err != nil {
		return fail("secrets git: %v", err)
	}
	return secrets.ScanGit(os.Stdout, dir, since)
}

func cmdSecretsDoctor([]string) int {
	reg, err := providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	return secrets.Doctor(os.Stdout, reg)
}

func cmdSecretsAll(args []string) int {
	rc := cmdSecretsScan(nil)
	fmt.Println()
	if gitRC := cmdSecretsGit(args); gitRC > rc {
		rc = gitRC
	}
	fmt.Println()
	if docRC := cmdSecretsDoctor(nil); docRC > rc {
		rc = docRC
	}
	return rc
}

// cmdSecretsImport stores a .env's NAME=value lines; any malformed line aborts before anything is written.
// "-" reads stdin instead of a file: the usual TTY confirm can't read from the data stream, so "-" requires
// --yes and is refused outright when stdin is itself a terminal (nothing piped in).
func cmdSecretsImport(args []string) int {
	yes, rest := extractBool(args, "yes")
	if len(rest) != 1 || rest[0] == "-h" || rest[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets import <file.env>|- [--yes]  (NAME=value lines)")
		return 2
	}
	name := rest[0]
	var r io.Reader
	if name == "-" {
		if tui.StdinIsTerminal() {
			return fail("secrets import -: refusing, stdin is a terminal (pipe data in)")
		}
		if !yes {
			return fail("secrets import -: needs --yes (no confirm prompt possible on piped stdin)")
		}
		r = os.Stdin
	} else {
		f, err := os.Open(name)
		if err != nil {
			return fail("secrets import: %v", err)
		}
		defer f.Close()
		r = f
	}
	pairs, problems := secrets.ParseDotenv(r)
	if len(problems) > 0 {
		return fail("secrets import: %s: nothing written\n  %s", name, strings.Join(problems, "\n  "))
	}
	if len(pairs) == 0 {
		return fail("secrets import: %s has no NAME=value lines", name)
	}
	for _, p := range pairs {
		if err := tailscale.ValidCredential(p.Name, p.Value); err != nil {
			return fail("secrets import: %s: %v", name, err)
		}
	}
	for _, p := range pairs {
		fmt.Fprintf(os.Stderr, "  %-28s %s\n", p.Name, secrets.Preview(p.Value))
	}
	if tui.StdinIsTerminal() && !tui.ConfirmKey(fmt.Sprintf("Store %d secret(s)?", len(pairs)), true) {
		fmt.Fprintln(os.Stderr, "secrets import: nothing written")
		return 1
	}
	for _, p := range pairs {
		if err := secrets.Store(p.Name, p.Value); err != nil {
			return fail("secrets import: %s: %v", p.Name, err)
		}
		fmt.Fprintf(os.Stderr, "stored %s in the vault\n", p.Name)
	}
	return 0
}

// cmdSecretsExport appends stored secrets (all, or the NAMEs given) to --out (default ./.env): keys the file
// already defines are skipped, and the file is left 0600 whether it was created or appended to. --out - writes
// the NAME=value lines to stdout instead, with no file and no plaintext ever touching disk.
func cmdSecretsExport(args []string) int {
	out, rest, err := extractValue(args, "out")
	if err != nil || slices.Contains(rest, "-h") || slices.Contains(rest, "--help") {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets export [NAME...] [--out file|-]")
		fmt.Fprintln(os.Stderr, "  file: appends, 0600 (default ./.env)   -: stream to stdout")
		return 2
	}
	if out == "" {
		out = ".env"
	}
	pairs, err := secrets.ExportPairs(rest)
	if err != nil {
		return fail("secrets %v", err)
	}
	if len(pairs) == 0 {
		return fail("secrets export: no stored secrets to export")
	}
	if out == "-" {
		var b strings.Builder
		for _, p := range pairs {
			b.WriteString(p.Name + "=" + p.Value + "\n")
		}
		if _, err := io.WriteString(os.Stdout, b.String()); err != nil {
			return fail("secrets export: %v", err)
		}
		fmt.Fprintf(os.Stderr, "wrote %d secret(s) to stdout\n", len(pairs))
		return 0
	}
	added, skipped, err := secrets.AppendDotenv(out, pairs)
	if err != nil {
		return fail("secrets export: %v", err)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "kept %s (already in %s)\n", strings.Join(skipped, ", "), out)
	}
	fmt.Fprintf(os.Stderr, "wrote %d secret(s) to %s (0600, plaintext — keep it out of git)\n", added, out)
	return 0
}

// cmdSecretsList prints each vault secret (never the process env) with a masked value; nothing is decrypted.
func cmdSecretsList(args []string) int {
	if slices.Contains(args, "-h") || slices.Contains(args, "--help") {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets list [NAME...]")
		return 2
	}
	rows, err := secrets.ListStored(args)
	if err != nil {
		return fail("secrets list: %v", err)
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no stored secrets")
		return 0
	}
	w := 0
	for _, r := range rows {
		w = max(w, len(r.Name))
	}
	for _, r := range rows {
		fmt.Printf("%-*s  %-7s  %s\n", w, r.Name, r.Source, r.Masked())
	}
	return 0
}
