package secrets

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

// dotenvName is the only key shape import accepts: an env var name (letters, digits, _; not leading a digit).
var dotenvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Pair is one NAME=value line.
type Pair struct{ Name, Value string }

// ParseDotenv reads strict NAME=value lines, skipping blanks and lines starting with #. The value is everything
// after the first = verbatim (no quotes, no export). Every other line is a "line N: …" problem; nothing is guessed.
func ParseDotenv(r io.Reader) (pairs []Pair, problems []string) {
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		switch {
		case !ok || !dotenvName.MatchString(name):
			problems = append(problems, fmt.Sprintf("line %d: want NAME=value", n))
		case value == "":
			problems = append(problems, fmt.Sprintf("line %d: %s has no value", n, name))
		default:
			pairs = append(pairs, Pair{name, value})
		}
	}
	return pairs, problems
}

// Stored is one secret the harness holds: its name, the plane it resolved from, and the value.
type Stored struct{ Name, Source, Value string }

// ExportPairs is every stored secret as NAME=value pairs (single-line values only), restricted to only when given.
func ExportPairs(reg *providers.Registry, only []string) []Pair {
	var out []Pair
	for _, s := range ListStored(reg, only) {
		if !strings.ContainsAny(s.Value, "\r\n") {
			out = append(out, Pair{s.Name, s.Value})
		}
	}
	return out
}

// ListStored is every secret the keyring or the file plane holds (indexed names, file keys, provider auth.env
// names), restricted to only when given; the process env is not ours to list or export.
func ListStored(reg *providers.Registry, only []string) []Stored {
	names := slices.Clone(Names())
	for k := range ReadAll() {
		names = append(names, k)
	}
	if reg != nil {
		for _, p := range reg.Providers {
			if p.Auth.Env != "" {
				names = append(names, p.Auth.Env)
			}
		}
	}
	slices.Sort(names)
	var out []Stored
	for _, n := range slices.Compact(names) {
		if len(only) > 0 && !slices.Contains(only, n) {
			continue
		}
		if v, src, ok := Get(n); ok && src != "env" {
			out = append(out, Stored{n, src, v})
		}
	}
	return out
}

// FormatDotenv renders pairs as NAME=value lines under a header comment.
func FormatDotenv(pairs []Pair) string {
	var b strings.Builder
	b.WriteString("# claude-code secrets export — plaintext credentials; keep it private and delete it after use.\n")
	for _, p := range pairs {
		b.WriteString(p.Name + "=" + p.Value + "\n")
	}
	return b.String()
}
