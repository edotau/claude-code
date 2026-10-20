package secrets

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
	if err := sc.Err(); err != nil { // e.g. a line past the scanner's 64 KiB limit: nothing after it was read
		problems = append(problems, fmt.Sprintf("read stopped early: %v", err))
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

// ListStored is every secret the keyring or the file plane holds (meta names, file keys, provider auth.env
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

// AppendDotenv appends pairs to path as NAME=value lines, creating it (with a header) when absent and skipping names
// it already defines; the file ends 0600 either way. added counts lines written, skipped names the kept ones.
func AppendDotenv(path string, pairs []Pair) (added int, skipped []string, err error) {
	have := map[string]bool{}
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		for line := range strings.SplitSeq(string(existing), "\n") {
			if name, _, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
				have[strings.TrimSpace(strings.TrimPrefix(name, "export "))] = true
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return 0, nil, err
	}
	var b strings.Builder
	if len(existing) == 0 {
		b.WriteString(dotenvHeader)
	} else if !strings.HasSuffix(string(existing), "\n") {
		b.WriteString("\n")
	}
	for _, p := range pairs {
		if have[p.Name] {
			skipped = append(skipped, p.Name)
			continue
		}
		b.WriteString(p.Name + "=" + p.Value + "\n")
		added++
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, nil, err
	}
	if err := f.Chmod(0o600); err != nil { // an existing .env keeps its old mode until now: tighten before writing
		return 0, nil, errors.Join(err, f.Close())
	}
	if _, err := f.WriteString(b.String()); err != nil {
		return 0, nil, errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return 0, nil, err
	}
	return added, skipped, os.Chmod(path, 0o600)
}

// dotenvHeader opens a .env that export creates.
const dotenvHeader = "# claude-code secrets export — plaintext credentials; keep this file private and out of git.\n"
