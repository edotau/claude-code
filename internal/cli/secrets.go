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
	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
)

func init() { secretsRun = cmdSecrets }

const secretsUsage = `usage: claude-code secrets <op> [flags]
  set [<NAME>] [--file]   store a secret (keyring; --file = env.d/secrets.env)
                          NAME: prompts for the value; no NAME: guided dialog
  get <NAME> [--export]   print a secret (keyring -> secrets.env -> env)
  delete <NAME>           remove a secret from the keyring and secrets.env
  import <file> [--file]  store every NAME=value line of a .env (# lines skipped)
  export [NAME...] [--out f] append stored secrets to ./.env or f (0600)
  list [NAME...]          stored secrets: name, keyring|file, masked value
  status                  credential source per provider (never the value)
  scan [paths...]         find plaintext secrets in config files
  git [--since N]         find secret-shaped additions in git history
  doctor                  file perms, key names, duplicates, keyring tool
  all                     scan + git + doctor
`

// cmdSecrets dispatches the `secrets` verb.
func cmdSecrets(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, secretsUsage)
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
	fmt.Fprint(os.Stderr, secretsUsage)
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
	forceFile, rest := extractBool(args, "file")
	dryRun, rest := extractBool(rest, "dry-run")
	if slices.Contains(rest, "--help") || slices.Contains(rest, "-h") {
		fmt.Fprint(os.Stderr, secretsUsage)
		return 2
	}
	if len(rest) == 0 && !dryRun && tui.StdinIsTerminal() {
		return configure.Secrets(configure.Options{In: os.Stdin, Out: os.Stdout, Interactive: true}, forceFile)
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets set [<NAME>] [--file] [--dry-run]   (value on stdin; no NAME on a terminal = dialog)")
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
	if dryRun {
		plane, verb := secrets.EnvFile(), "add"
		if !forceFile && keyring.Available() {
			plane = "the OS keyring"
		} else if _, ok := secrets.ReadAll()[name]; ok {
			verb = "replace"
		}
		fmt.Printf("would %s %s in %s (%d chars); nothing written\n", verb, name, plane, len(value))
		return 0
	}
	plane, err := secrets.Store(name, value, forceFile)
	if err != nil {
		return fail("secrets set: %v", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s to %s (%s); `claude-code secrets get %s` prints it\n", name, plane, secrets.Redact(value), name)
	return 0
}

// cmdSecretsDelete removes NAME from every managed plane (keyring, secrets.env, names index).
func cmdSecretsDelete(args []string) int {
	if len(args) != 1 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets delete <NAME>")
		return 2
	}
	name := args[0]
	if _, _, ok := secrets.Get(name); !ok {
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
	value, _, ok := secrets.Get(name)
	if !ok {
		return fail("secrets get: %s not set (checked the OS keyring, env.d/secrets.env, and the process env)", name)
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
func cmdSecretsImport(args []string) int {
	forceFile, rest := extractBool(args, "file")
	if len(rest) != 1 || rest[0] == "-h" || rest[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets import <file.env> [--file]   (NAME=value lines; # lines skipped)")
		return 2
	}
	f, err := os.Open(rest[0])
	if err != nil {
		return fail("secrets import: %v", err)
	}
	defer f.Close()
	pairs, problems := secrets.ParseDotenv(f)
	if len(problems) > 0 {
		return fail("secrets import: %s: nothing written\n  %s", rest[0], strings.Join(problems, "\n  "))
	}
	if len(pairs) == 0 {
		return fail("secrets import: %s has no NAME=value lines", rest[0])
	}
	for _, p := range pairs {
		fmt.Fprintf(os.Stderr, "  %-28s %s\n", p.Name, secrets.Preview(p.Value))
	}
	if tui.StdinIsTerminal() && !tui.ConfirmKey(fmt.Sprintf("Store %d secret(s)?", len(pairs)), true) {
		fmt.Fprintln(os.Stderr, "secrets import: nothing written")
		return 1
	}
	for _, p := range pairs {
		plane, err := secrets.Store(p.Name, p.Value, forceFile)
		if err != nil {
			return fail("secrets import: %s: %v", p.Name, err)
		}
		audit.Log(audit.SecretSet, p.Name, plane)
		fmt.Fprintf(os.Stderr, "stored %s in %s\n", p.Name, plane)
	}
	return 0
}

// cmdSecretsExport appends stored secrets (all, or the NAMEs given) to --out (default ./.env): keys the file
// already defines are skipped, and the file is left 0600 whether it was created or appended to.
func cmdSecretsExport(args []string) int {
	out, rest, err := extractValue(args, "out")
	if err != nil || slices.Contains(rest, "-h") || slices.Contains(rest, "--help") {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets export [NAME...] [--out file]   (default ./.env; appends, 0600)")
		return 2
	}
	if out == "" {
		out = ".env"
	}
	reg, _ := providers.Load()
	pairs := secrets.ExportPairs(reg, rest)
	if len(pairs) == 0 {
		return fail("secrets export: no stored secrets to export")
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

// cmdSecretsList prints each stored secret (keyring or file plane, never the process env) with a masked value.
func cmdSecretsList(args []string) int {
	if slices.Contains(args, "-h") || slices.Contains(args, "--help") {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets list [NAME...]")
		return 2
	}
	reg, _ := providers.Load()
	rows := secrets.ListStored(reg, args)
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no stored secrets")
		return 0
	}
	w := 0
	for _, r := range rows {
		w = max(w, len(r.Name))
	}
	for _, r := range rows {
		// name + plane + preview within 80 columns: the preview gets what the name column leaves (≥8 stars)
		room := max(8, 80-w-2-7-2-13) // 13 = " (NNN chars)"
		fmt.Printf("%-*s  %-7s  %s\n", w, r.Name, r.Source, secrets.PreviewWithin(r.Value, room))
	}
	return 0
}
