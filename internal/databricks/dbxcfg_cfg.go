// Adapted from github.com/databricks/databricks-sdk-go v0.190.0 config/config_file.go; modified.

// The dbxcfg files read a Databricks CLI config ONCE into vault provider rows and write rows back out secret-free.

package databricks

import (
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

// profile is one parsed section including secrets; it never leaves this package.
type profile struct {
	name, host, authType, clientID, clientSecret, token, accountID, workspaceID string
}

// DefaultPath is $DATABRICKS_CONFIG_FILE (a leading ~ expanded), else the CLI's own ~/.databrickscfg.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	if p := os.Getenv(EnvConfigFile); p != "" {
		if rest, ok := strings.CutPrefix(p, "~"); ok && home != "" {
			return home + rest
		}
		return p
	}
	if home == "" {
		return ".databrickscfg"
	}
	return filepath.Join(home, ".databrickscfg")
}

// section is one merged [name]: a repeated header merges into the first, a repeated key keeps the last value.
type section struct {
	name string
	keys map[string]string
}

// isReserved reports DEFAULT (any case) and __settings__, which are never imported.
func isReserved(name string) bool {
	return strings.EqualFold(name, defaultSection) || name == settingsSection
}

// parseSections reads databrickscfg text the way the SDK's ini loader (SpaceBeforeInlineComment) does.
// Keys above the first header belong to DEFAULT; a line without a delimiter is skipped.
func parseSections(text string) []*section {
	lines := strings.Split(strings.TrimPrefix(text, "\ufeff"), "\n")
	var order []*section
	byName := map[string]*section{}
	cur := func(name string) *section {
		if s, ok := byName[name]; ok {
			return s
		}
		s := &section{name: name, keys: map[string]string{}}
		byName[name] = s
		order = append(order, s)
		return s
	}
	at := cur(defaultSection)
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(strings.TrimSuffix(lines[i], "\r"))
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			if name, ok := headerName(line); ok {
				at = cur(name)
			}
			continue
		}
		idx := strings.IndexAny(line, "=:")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		var val string
		val, i = readValue(line[idx+1:], lines, i)
		at.keys[key] = val
	}
	if len(byName[defaultSection].keys) == 0 {
		order = order[1:] // an empty implicit DEFAULT is not a section
	}
	return order
}

// headerName parses `[name]`, ignoring a trailing `# c` / `; c` comment.
func headerName(line string) (string, bool) {
	end := strings.IndexByte(line, ']')
	if end < 0 {
		return "", false
	}
	if rest := strings.TrimSpace(line[end+1:]); rest != "" && rest[0] != '#' && rest[0] != ';' {
		return "", false
	}
	name := strings.TrimSpace(line[1:end])
	return name, name != ""
}

// readValue mirrors go-ini: """/` quoted (may span lines), `\` continuation, ` #`/` ;` inline comments,
// then one pair of surrounding quotes trimmed. It returns the value and the index of the last line consumed.
func readValue(raw string, lines []string, i int) (string, int) {
	v := strings.TrimLeft(raw, " \t")
	for _, q := range []string{`"""`, "`"} {
		if !strings.HasPrefix(v, q) || (q == `"""` && len(v) <= 3) {
			continue
		}
		body := v[len(q):]
		for {
			if j := strings.LastIndex(body, q); j >= 0 {
				return body[:j], i
			}
			if i+1 >= len(lines) {
				return body, i
			}
			i++
			body += "\n" + strings.TrimSuffix(lines[i], "\r")
		}
	}
	v = strings.TrimSpace(v)
	for strings.HasSuffix(v, `\`) && i+1 < len(lines) {
		i++
		v = v[:len(v)-1] + strings.TrimSpace(strings.TrimSuffix(lines[i], "\r"))
	}
	if j := strings.Index(v, " #"); j >= 0 {
		v = strings.TrimSpace(v[:j])
	} else if j := strings.Index(v, " ;"); j >= 0 {
		v = strings.TrimSpace(v[:j])
	}
	for _, q := range []string{`"`, "'"} {
		if len(v) >= 2 && strings.HasPrefix(v, q) && strings.HasSuffix(v, q) && strings.Count(v, q) == 2 {
			return v[1 : len(v)-1], i
		}
	}
	return v, i
}

func toProfile(s *section) profile {
	k := s.keys
	return profile{name: s.name, host: k["host"], authType: k["auth_type"], clientID: k["client_id"],
		clientSecret: k["client_secret"], token: k["token"], accountID: k["account_id"], workspaceID: k["workspace_id"]}
}

// parse returns the named profiles of databrickscfg text plus whether a non-empty [DEFAULT] exists.
func parse(r io.Reader) ([]profile, bool, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, false, err
	}
	var out []profile
	hasDefault := false
	for _, s := range parseSections(string(b)) {
		if strings.EqualFold(s.name, defaultSection) && len(s.keys) > 0 {
			hasDefault = true
		}
		if !isReserved(s.name) {
			out = append(out, toProfile(s))
		}
	}
	return out, hasDefault, nil
}

func load(path string) ([]profile, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	return parse(f)
}
