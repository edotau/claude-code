package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
)

func init() { secretsRun = cmdSecrets }

const secretsUsage = `usage: claude-code secrets <op> [flags]
  set <NAME> [--keyring]   write a secret (value on stdin) to env.d/secrets.env, or the OS keyring
  get <NAME> [--export]    resolve: keyring -> env.d/secrets.env -> process env
  status                   every registered provider's credential source (never the value)
  scan [paths...]          audit config files for inline plaintext secrets (default: the known surface)
  git [--since N]          audit this directory's git history for secret-shaped additions (read-only)
  doctor                   file perms, key names, duplicate keys, keyring tool presence
  all                      scan + git + doctor
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
	useKeyring, rest := extractBool(args, "keyring")
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-code secrets set <NAME> [--keyring]   (value on stdin)")
		return 2
	}
	name := rest[0]
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail("secrets set: read stdin: %v", err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return fail("secrets set: empty value on stdin")
	}
	if useKeyring {
		if err := secrets.KeyringSet(name, value); err != nil {
			return fail("%v", err)
		}
		fmt.Fprintf(os.Stderr, "wrote %s to the OS keyring (%d chars)\n", name, len(value))
		return 0
	}
	if err := secrets.SetFile(name, value); err != nil {
		return fail("secrets set: %v", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s to %s (%d chars, 0600)\n", name, secrets.EnvFile(), len(value))
	return 0
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
