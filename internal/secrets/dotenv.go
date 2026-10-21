package secrets

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/vault"
)

// dotenvName is the only key shape import accepts: an env var name (letters, digits, _; not leading a digit).
var dotenvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Pair is one NAME=value line.
type Pair struct{ Name, Value string }

// ParseDotenv reads strict NAME=value lines (optional `export `, one pair of matching quotes stripped), skipping
// blanks and # lines; every other line is a "line N: …" problem, and a read error is one too — nothing is guessed.
func ParseDotenv(r io.Reader) (pairs []Pair, problems []string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		value = strings.TrimSpace(value) // match providers.ReadEnvFile: trim, then strip one quote pair
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		switch {
		case !ok || !dotenvName.MatchString(name):
			problems = append(problems, fmt.Sprintf("line %d: want NAME=value", n))
		case value == "":
			problems = append(problems, fmt.Sprintf("line %d: %s has no value", n, name))
		default:
			pairs = append(pairs, Pair{name, value})
		}
	}
	if err := sc.Err(); err != nil { // e.g. a line past the 4 MiB limit: nothing after it was read
		problems = append(problems, fmt.Sprintf("read stopped early: %v", err))
	}
	return pairs, problems
}

// Stored is one vault secret's display metadata, never its value.
type Stored struct {
	Name, Source, Prefix string
	Len                  int
}

func (s Stored) Masked() string {
	if s.Prefix == "" {
		return fmt.Sprintf("%d chars", s.Len)
	}
	return fmt.Sprintf("%s*** (%d chars)", s.Prefix, s.Len)
}

// ExportPairs decrypts the selected vault secrets; any failure discards all pairs before a caller can write them.
func ExportPairs(only []string) ([]Pair, error) {
	stored, err := ListStored(only)
	if err != nil {
		return nil, err
	}
	var out []Pair
	for _, s := range stored {
		value, found, err := vault.ReadSecret(s.Name)
		if err != nil {
			return nil, fmt.Errorf("export: %s: %w", s.Name, err)
		}
		if !found {
			return nil, fmt.Errorf("export: %s: no longer stored in the vault", s.Name)
		}
		out = append(out, Pair{s.Name, value})
	}
	return out, nil
}

// ListStored lists only vault records, filtered and sorted, without decrypting or including process env values.
func ListStored(only []string) ([]Stored, error) {
	meta, err := LoadMeta()
	if err != nil {
		return nil, err
	}
	var out []Stored
	for _, n := range slices.Sorted(maps.Keys(meta)) {
		if len(only) > 0 && !slices.Contains(only, n) {
			continue
		}
		m := meta[n]
		out = append(out, Stored{Name: n, Source: "vault", Prefix: m.Prefix, Len: m.Len})
	}
	return out, nil
}

// AppendDotenv appends pairs to path as NAME=value lines, creating it (with a header) when absent and skipping names
// it already defines; the file ends 0600 either way. added counts lines written, skipped names the kept ones.
// path is refused when it is a symlink — this must never write through one to an arbitrary target.
func AppendDotenv(path string, pairs []Pair) (added int, skipped []string, err error) {
	if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return 0, nil, fmt.Errorf("%s is a symlink; refusing to write through it", path)
	}
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
		b.WriteString(p.Name + "=" + quoteDotenvValue(p.Value) + "\n")
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
	if err := os.Chmod(path, 0o600); err != nil {
		return 0, nil, err
	}
	warnIfUntrackedDefault(path)
	return added, skipped, nil
}

// quoteDotenvValue wraps v in quotes when it holds a space, `#`, `$` or a quote character; ParseDotenv
// strips exactly one matching outer quote pair with no escaping, so wrapping (whichever quote char, picking
// one absent from v when possible) always round-trips the original value unchanged.
func quoteDotenvValue(v string) string {
	if v == "" || !strings.ContainsAny(v, " \t\"'$#") {
		return v
	}
	if !strings.Contains(v, "'") {
		return "'" + v + "'"
	}
	return `"` + v + `"`
}

// warnIfUntrackedDefault warns on stderr when path is the default ./.env and a git work tree here does
// not gitignore it; git absent, not a repo, or already ignored all skip silently — nothing to warn about.
func warnIfUntrackedDefault(path string) {
	if path != ".env" {
		return
	}
	// Exit 1 alone means "not ignored"; 0 is ignored and 128 is not a work tree (or another git failure).
	var exit *exec.ExitError
	if err := exec.Command("git", "check-ignore", "-q", path).Run(); !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return
	}
	fmt.Fprintf(os.Stderr, "claude-code: warning: %s is not covered by .gitignore here; it holds plaintext secrets\n", path)
}

// dotenvHeader opens a .env that export creates.
const dotenvHeader = "# claude-code secrets export — plaintext credentials; keep this file private and out of git.\n"
