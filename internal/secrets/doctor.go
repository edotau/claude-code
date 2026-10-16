package secrets

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

// Doctor runs the offline coherence checks: file perms, key names against the provider registry,
// duplicate keys, and whether a keyring tool exists on this box. Never prints a value.
func Doctor(out io.Writer, reg *providers.Registry) int {
	fmt.Fprintln(out, "== secrets doctor ==")
	fail := false
	fail = checkPerms(out) || fail
	fail = checkDuplicateKeys(out) || fail
	checkKeyNames(out, reg) // advisory only, never fails the gate
	checkKeyringTool(out)   // informational only

	if fail {
		fmt.Fprintln(out, "secrets doctor: problem(s) found.")
		return ExitFindings
	}
	fmt.Fprintln(out, "secrets doctor: clean.")
	return ExitClean
}

// checkPerms flags a secrets.env file readable/writable by anyone but its owner.
func checkPerms(out io.Writer) bool {
	file := EnvFile()
	fi, err := os.Stat(file)
	if os.IsNotExist(err) {
		fmt.Fprintln(out, "  ✓ "+file+" does not exist yet (nothing to check)")
		return false
	}
	if err != nil {
		fmt.Fprintf(out, "  ⚠ could not stat %s: %v\n", file, err)
		return true
	}
	if fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(out, "  ⚠ %s is %04o, want 0600 — run: chmod 600 %s\n", file, fi.Mode().Perm(), file)
		return true
	}
	fmt.Fprintln(out, "  ✓ "+file+" is 0600")
	return false
}

// checkDuplicateKeys flags a KEY= line repeated in secrets.env (the second write silently wins,
// which ReadEnvFile's map can't surface — only a raw line scan can).
func checkDuplicateKeys(out io.Writer) bool {
	file := EnvFile()
	f, err := os.Open(file)
	if err != nil {
		return false // missing/unreadable already reported by checkPerms
	}
	defer f.Close()
	seen := map[string]bool{}
	var dupes []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if seen[k] {
			dupes = append(dupes, k)
			continue
		}
		seen[k] = true
	}
	if len(dupes) == 0 {
		return false
	}
	for _, k := range dupes {
		fmt.Fprintf(out, "  ⚠ %s is set twice in %s — only the last line wins\n", k, file)
	}
	return true
}

// checkKeyNames warns about a stored key that matches no provider's auth.env — likely a typo, but
// not necessarily wrong (a provider not yet in the registry), so this never fails the gate.
func checkKeyNames(out io.Writer, reg *providers.Registry) {
	known := map[string]bool{}
	for _, p := range reg.Providers {
		if p.Auth.Env != "" {
			known[p.Auth.Env] = true
		}
	}
	for k := range ReadAll() {
		if !known[k] {
			fmt.Fprintf(out, "  ⚠ %s matches no provider's auth.env in the registry — typo?\n", k)
		}
	}
}

// checkKeyringTool is informational: its absence just means the file plane is the only option.
func checkKeyringTool(out io.Writer) {
	if KeyringAvailable() {
		fmt.Fprintln(out, "  ✓ a keyring tool is on PATH ("+keyringBin()+")")
		return
	}
	fmt.Fprintln(out, "  ℹ no keyring tool on PATH for this OS — `secrets set --keyring` is unavailable")
}
