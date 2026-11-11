// The ini parser reads a Databricks CLI config once into vault provider rows; export writes them back secret-free.

package databricks

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

const (
	defaultSection  = "DEFAULT"      // the CLI's fallback profile; its values never inherit into named profiles
	settingsSection = "__settings__" // the CLI's own bookkeeping block, not a profile
	// envConfigFile is the CLI's own variable, read by name only.
	envConfigFile = "DATABRICKS_CONFIG_FILE"
)

// defaultPath is $DATABRICKS_CONFIG_FILE (a leading ~ expanded), else the CLI's own ~/.databrickscfg.
func defaultPath() string {
	home, _ := os.UserHomeDir()
	p := os.Getenv(envConfigFile)
	if p == "" {
		return filepath.Join(home, ".databrickscfg")
	}
	return paths.ExpandHome(p)
}

// section is one [name] block with its keys; a repeated key or header merges, last value wins.
type section struct {
	name    string
	keys    map[string]string
	pair    string
	pairSet bool
}

func isReserved(name string) bool {
	return strings.EqualFold(name, defaultSection) || name == settingsSection
}

// iniState is parseSections' running state: sections, the current one, a pending pairing comment, and line notes.
type iniState struct {
	out      []*section
	byName   map[string]*section
	cur      *section
	pair     string
	pairSet  bool
	skipping bool // inside an unterminated header's block: its keys belong to no section
	notes    []string
	sc       *lineScanner
}

// lineScanner counts the lines it scans so notes can name them.
type lineScanner struct {
	*bufio.Scanner
	n int
}

func (l *lineScanner) Scan() bool {
	l.n++
	return l.Scanner.Scan()
}

// note records one skipped or merged line as prefix + "line N" + the formatted rest.
func (st *iniState) note(prefix, format string, a ...any) {
	st.notes = append(st.notes, fmt.Sprintf("%sline %d", prefix, st.sc.n)+fmt.Sprintf(format, a...))
}

// enter makes name the current section, creating it on first appearance.
func (st *iniState) enter(name string) {
	st.skipping = false
	if st.cur = st.byName[name]; st.cur == nil {
		st.cur = &section{name: name, keys: map[string]string{}}
		st.byName[name] = st.cur
		st.out = append(st.out, st.cur)
	}
}

// header handles a "[name]" line: the pending pairing comment attaches to that section.
func (st *iniState) header(name string) {
	if st.byName[name] != nil {
		st.note("", ": section [%s] repeated; its keys merge into the first, the last value wins", name)
	}
	st.enter(name)
	if st.pairSet {
		st.cur.pair, st.cur.pairSet = st.pair, true
	}
	st.pair, st.pairSet = "", false
}

// keyLine handles a "k = v" line; keys above the first header belong to DEFAULT.
func (st *iniState) keyLine(line string) {
	st.pair, st.pairSet = "", false
	if st.skipping {
		return
	}
	if st.cur == nil {
		st.enter(defaultSection)
	}
	i := strings.IndexAny(line, "=:")
	if i <= 0 {
		st.note("unable to parse ", " in [%s]: no \"=\" separator; line skipped", st.cur.name)
		return
	}
	k := strings.TrimSpace(line[:i])
	if _, dup := st.cur.keys[k]; dup {
		st.note("", " in [%s]: key %q repeated; the last value wins", st.cur.name, k)
	}
	st.cur.keys[k] = readValue(line[i+1:], st.sc)
}

// parseSections scans INI text into sections (headerless keys go to DEFAULT) plus a note per skipped or merged line.
func parseSections(text string) ([]*section, []string, error) {
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	st := &iniState{byName: map[string]*section{}, sc: &lineScanner{Scanner: sc}}
	for st.sc.Scan() {
		line := sc.Text()
		if st.sc.n == 1 {
			line = strings.TrimPrefix(line, "\ufeff") // a UTF-8 BOM would hide the first header
		}
		line = strings.TrimSpace(line)
		if v, ok := pairingComment(line); ok {
			st.pair, st.pairSet = v, true
			continue
		}
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if name, ok := headerName(line); ok {
			st.header(name)
		} else if line[0] == '[' {
			st.note("unable to parse ", ": unterminated section header; its keys up to the next header are skipped")
			st.skipping, st.pair, st.pairSet = true, "", false
		} else {
			st.keyLine(line)
		}
	}
	return st.out, st.notes, sc.Err()
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
func readValue(raw string, sc *lineScanner) string {
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

// parse returns the named profiles (DEFAULT and __settings__ excluded), whether DEFAULT has keys, and line notes.
func parse(text string) ([]*section, bool, []string, error) {
	var ps []*section
	hasDefault := false
	secs, notes, err := parseSections(text)
	if err != nil {
		return nil, false, nil, err
	}
	for _, s := range secs {
		switch {
		case strings.EqualFold(s.name, defaultSection):
			hasDefault = hasDefault || len(s.keys) > 0
		case isReserved(s.name):
		default:
			ps = append(ps, s)
		}
	}
	return ps, hasDefault, notes, nil
}

func load(path string) ([]*section, bool, []string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, nil, err
	}
	return parse(string(b))
}

// pairingComment recognizes the portable pairing signal above an SP section; none means explicitly unpaired.
func pairingComment(line string) (string, bool) {
	v, ok := strings.CutPrefix(strings.TrimSpace(line), "# claude-code: pair=")
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "none" {
		v = ""
	}
	return v, true
}

// The .databrickscfg auth_type spellings; rows call them providers.AuthOAuth, AuthM2M and AuthPAT.
const (
	cliAuthU2M = "databricks-cli"
	cliAuthM2M = "oauth-m2m"
	cliAuthPAT = "pat"
)

// cfgCandidate is one importable profile: identifying fields and secret PRESENCE only, structurally no secret.
type cfgCandidate struct {
	Pair        string // source user profile named by the pairing comment
	PairSet     bool   // includes an explicit pair=none
	Env         string // optional dev/qa/prod tag
	Name        string
	Host        string // scheme+host only; "" when the source host is unusable
	ClientID    string // an identifier, not a secret
	AccountID   string // account_id, an identifier
	WorkspaceID string // workspace_id, an identifier
	HasSecret   bool   // a client_secret or PAT exists in the source file
	Auth        string // row auth.type (oauth|m2m|pat), "" when Skip is set
	Skip        string // why the profile cannot become a row
}

// defaultOnlySkip explains why a file holding only [DEFAULT] imports nothing.
const defaultOnlySkip = "[DEFAULT] is the CLI's fallback, never imported; copy it into a named profile"

// readCandidates lists path's profiles plus parser notes; DEFAULT is a skip row when alone, else a note.
func readCandidates(path string) ([]cfgCandidate, []string, error) {
	ps, hasDefault, notes, err := load(path)
	if err != nil {
		return nil, nil, err
	}
	out := make([]cfgCandidate, 0, len(ps))
	for _, p := range ps {
		out = append(out, classifyProfile(p))
	}
	switch {
	case len(out) == 0 && hasDefault:
		out = append(out, cfgCandidate{Name: defaultSection, Skip: defaultOnlySkip})
	case hasDefault:
		notes = append(notes, "[DEFAULT] skipped: the CLI's fallback, never imported; copy it into a named profile")
	}
	return out, notes, nil
}

// classifyProfile maps a profile to a row authType; an explicit auth_type wins over inference from the keys present.
func classifyProfile(s *section) cfgCandidate {
	k := s.keys
	c := cfgCandidate{Pair: s.pair, PairSet: s.pairSet, Env: k["env"], Name: s.name, ClientID: k["client_id"],
		AccountID: k["account_id"], WorkspaceID: k["workspace_id"]}
	host, ok := normalizeHost(k["host"])
	c.Host = host
	c.Auth, c.Skip = rowAuth(k, effectiveCLIAuth(k), ok)
	// only m2m seals a client_secret and pat a token from the file
	c.HasSecret = c.Auth == providers.AuthM2M && k["client_secret"] != "" || c.Auth == providers.AuthPAT && k["token"] != ""
	return c
}

// effectiveCLIAuth is the explicit auth_type, else inferred from the credential keys present.
func effectiveCLIAuth(k map[string]string) string {
	switch {
	case k["auth_type"] != "":
		return k["auth_type"]
	case k["client_id"] != "" && k["client_secret"] != "":
		return cliAuthM2M
	case k["token"] != "":
		return cliAuthPAT
	}
	return cliAuthU2M
}

// rowAuth maps a CLI auth type to a row auth, or the reason the profile is skipped.
func rowAuth(k map[string]string, auth string, hostOK bool) (row, skip string) {
	switch {
	case !hostOK:
		return "", "no usable https host"
	case auth == cliAuthU2M:
		return providers.AuthOAuth, ""
	case auth == cliAuthM2M && k["client_id"] == "":
		return "", "oauth-m2m profile without client_id"
	case auth == cliAuthM2M:
		return providers.AuthM2M, ""
	case auth == cliAuthPAT:
		return providers.AuthPAT, ""
	}
	return "", fmt.Sprintf("unsupported auth_type %q", k["auth_type"])
}

// normalizeHost is oauth.NormalizeHost after dropping any path, query or fragment a cfg host carries.
func normalizeHost(h string) (string, bool) {
	h = strings.TrimSpace(h)
	_, rest, scheme := strings.Cut(h, "://")
	if !scheme {
		rest = h
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		h = h[:len(h)-len(rest)+i]
	}
	out := oauth.NormalizeHost(h)
	return out, out != ""
}

// cfgSecrets are the credential values of one profile; read only on an explicit --seal-secrets.
type cfgSecrets struct{ ClientSecret, Token string }

// readSecrets returns the named profile's secrets from path; DEFAULT and __settings__ are never readable.
func readSecrets(path, name string) (cfgSecrets, error) {
	ps, _, _, err := load(path)
	if err != nil {
		return cfgSecrets{}, err
	}
	for _, p := range ps {
		if p.name == name {
			return cfgSecrets{ClientSecret: p.keys["client_secret"], Token: p.keys["token"]}, nil
		}
	}
	return cfgSecrets{}, fmt.Errorf("profile %q not found", name)
}
