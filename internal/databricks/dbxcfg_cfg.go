// The dbxcfg files read a Databricks CLI config ONCE into vault provider rows and write rows back out secret-free.

package databricks

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultSection  = "DEFAULT"      // the CLI's fallback profile; its values never inherit into named profiles
	settingsSection = "__settings__" // the CLI's own bookkeeping block, not a profile
	// EnvConfigFile is the CLI's own variable, read by name only.
	EnvConfigFile = "DATABRICKS_CONFIG_FILE"
)

type profile struct {
	name, host, authType, clientID, clientSecret, token, accountID, workspaceID string
}

// DefaultPath is $DATABRICKS_CONFIG_FILE (a leading ~ expanded), else the CLI's own ~/.databrickscfg.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	p := os.Getenv(EnvConfigFile)
	switch {
	case p == "":
		return filepath.Join(home, ".databrickscfg")
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	}
	return p
}

// section is one [name] block with its keys; a repeated key or header merges, last value wins.
type section struct {
	name string
	keys map[string]string
}

func isReserved(name string) bool {
	return strings.EqualFold(name, defaultSection) || name == settingsSection
}

// parseSections scans INI text into sections in first-appearance order; keys before any header belong to DEFAULT.
func parseSections(text string) []*section {
	var out []*section
	byName := map[string]*section{}
	var cur *section
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for first := true; sc.Scan(); first = false {
		line := sc.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff") // a UTF-8 BOM would hide the first header
		}
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if name, ok := headerName(line); ok {
			if cur = byName[name]; cur == nil {
				cur = &section{name: name, keys: map[string]string{}}
				byName[name] = cur
				out = append(out, cur)
			}
			continue
		}
		if cur == nil { // keys above the first header belong to DEFAULT
			if cur = byName[defaultSection]; cur == nil {
				cur = &section{name: defaultSection, keys: map[string]string{}}
				byName[defaultSection] = cur
				out = append(out, cur)
			}
		}
		if i := strings.IndexAny(line, "=:"); i > 0 {
			cur.keys[strings.TrimSpace(line[:i])] = readValue(line[i+1:], sc)
		}
	}
	return out
}

// headerName returns the name of a "[name]" line, tolerating a trailing comment.
func headerName(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "[") {
		return "", false
	}
	end := strings.IndexByte(line, ']')
	if end < 0 {
		return "", false
	}
	if rest := strings.TrimSpace(line[end+1:]); rest != "" && rest[0] != '#' && rest[0] != ';' {
		return "", false
	}
	return strings.TrimSpace(line[1:end]), true
}

// readValue reads a value that may span lines: a triple-quoted or backtick block, or lines ending in a backslash.
func readValue(raw string, sc *bufio.Scanner) string {
	v := strings.TrimSpace(raw)
	for _, d := range []string{`"""`, `'''`, "`"} {
		if !strings.HasPrefix(v, d) {
			continue
		}
		rest := v[len(d):]
		for !strings.Contains(rest, d) && sc.Scan() {
			rest += "\n" + sc.Text()
		}
		if end := strings.Index(rest, d); end >= 0 {
			rest = rest[:end]
		}
		return rest
	}
	for strings.HasSuffix(v, `\`) && sc.Scan() {
		v = strings.TrimSuffix(v, `\`) + strings.TrimSpace(sc.Text())
	}
	return cleanValue(v)
}

// cleanValue trims a value, strips one pair of matching quotes, else cuts an inline " #" / " ;" comment.
func cleanValue(raw string) string {
	v := strings.TrimSpace(raw)
	if n := len(v); n >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
	}
	for i := 1; i < len(v); i++ {
		if (v[i] == '#' || v[i] == ';') && (v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}

func toProfile(s *section) profile {
	k := s.keys
	return profile{name: s.name, host: k["host"], authType: k["auth_type"], clientID: k["client_id"],
		clientSecret: k["client_secret"], token: k["token"], accountID: k["account_id"], workspaceID: k["workspace_id"]}
}

// parse returns the named profiles (DEFAULT and __settings__ excluded) and whether a DEFAULT section exists.
func parse(r io.Reader) ([]profile, bool, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, false, err
	}
	var ps []profile
	hasDefault := false
	for _, s := range parseSections(string(b)) {
		switch {
		case strings.EqualFold(s.name, defaultSection):
			hasDefault = hasDefault || len(s.keys) > 0
		case isReserved(s.name):
		default:
			ps = append(ps, toProfile(s))
		}
	}
	return ps, hasDefault, nil
}

func load(path string) ([]profile, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	return parse(f)
}
