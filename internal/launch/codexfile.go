package launch

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// codexFileOverrides flattens a config.toml (top-level keys and [a.b] tables, one-line values) into
// codex `-c key=value` pairs; values pass through as TOML, which codex parses itself.
func codexFileOverrides(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("-f: %w", err)
	}
	defer f.Close()
	var out []string
	table := ""
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "[["):
			return nil, fmt.Errorf("-f %s:%d: arrays of tables are not supported", path, n)
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			table = strings.TrimSpace(line[1:len(line)-1]) + "."
		default:
			k, v, ok := strings.Cut(line, "=")
			v = strings.TrimSpace(v)
			if !ok || v == "" || strings.HasSuffix(v, "[") || strings.HasSuffix(v, "{") || strings.HasPrefix(v, `"""`) {
				return nil, fmt.Errorf("-f %s:%d: want one-line `key = value`", path, n)
			}
			out = append(out, "-c", table+strings.TrimSpace(k)+"="+v)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("-f: %w", err)
	}
	return out, nil
}
