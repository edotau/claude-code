// The dbxcfg files read a Databricks CLI config ONCE into vault provider rows and write rows back out secret-free.

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
	// EnvConfigFile is the CLI's own variable, read by name only.
	EnvConfigFile = "DATABRICKS_CONFIG_FILE"
)

type profile struct {
	name, host, authType, clientID, clientSecret, token, accountID, workspaceID string
	pair, env                                                                   string
	pairSet                                                                     bool
}

// DefaultPath is $DATABRICKS_CONFIG_FILE (a leading ~ expanded), else the CLI's own ~/.databrickscfg.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	p := os.Getenv(EnvConfigFile)
	switch {
	case p == "":
		return filepath.Join(home, ".databrickscfg")
	case p == "~", strings.HasPrefix(p, "~/"):
		return paths.ExpandHome(p)
	}
	return p
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

// iniState is the running state of parseSections: sections so far, the current one, and a pending pairing comment.
type iniState struct {
	out     []*section
	byName  map[string]*section
	cur     *section
	pair    string
	pairSet bool
}

// enter makes name the current section, creating it on first appearance.
func (st *iniState) enter(name string) {
	if st.cur = st.byName[name]; st.cur == nil {
		st.cur = &section{name: name, keys: map[string]string{}}
		st.byName[name] = st.cur
		st.out = append(st.out, st.cur)
	}
}

// header handles a "[name]" line: the pending pairing comment attaches to that section.
func (st *iniState) header(name string) {
	st.enter(name)
	if st.pairSet {
		st.cur.pair, st.cur.pairSet = st.pair, true
	}
	st.pair, st.pairSet = "", false
}

// keyLine handles a "k = v" line; keys above the first header belong to DEFAULT.
func (st *iniState) keyLine(line string, sc *bufio.Scanner) {
	st.pair, st.pairSet = "", false
	if st.cur == nil {
		st.enter(defaultSection)
	}
	if i := strings.IndexAny(line, "=:"); i > 0 {
		st.cur.keys[strings.TrimSpace(line[:i])] = readValue(line[i+1:], sc)
	}
}

// parseSections scans INI text into sections in first-appearance order; keys before any header belong to DEFAULT.
// A scan error (a line over 1MB) is returned with the sections read so far.
func parseSections(text string) ([]*section, error) {
	st := &iniState{byName: map[string]*section{}}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for first := true; sc.Scan(); first = false {
		line := sc.Text()
		if first {
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
			continue
		}
		st.keyLine(line, sc)
	}
	return st.out, sc.Err()
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
		clientSecret: k["client_secret"], token: k["token"], accountID: k["account_id"], workspaceID: k["workspace_id"], pair: s.pair, pairSet: s.pairSet, env: k["env"]}
}

// parse returns the named profiles (DEFAULT and __settings__ excluded) and whether a DEFAULT section exists.
func parse(text string) ([]profile, bool, error) {
	var ps []profile
	hasDefault := false
	secs, err := parseSections(text)
	if err != nil {
		return nil, false, err
	}
	for _, s := range secs {
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
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
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
	CLIAuthU2M = "databricks-cli"
	CLIAuthM2M = "oauth-m2m"
	CLIAuthPAT = "pat"
)

// Candidate is one importable profile: identifying fields and secret PRESENCE only, structurally no secret.
type Candidate struct {
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

// Candidates enumerates the profiles of the config at path; a DEFAULT-only file lists DEFAULT as skipped.
func Candidates(path string) ([]Candidate, error) {
	ps, hasDefault, err := load(path)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(ps))
	for _, p := range ps {
		out = append(out, classifyProfile(p))
	}
	if len(out) == 0 && hasDefault {
		out = append(out, Candidate{Name: defaultSection, Skip: defaultOnlySkip})
	}
	return out, nil
}

// classifyProfile maps a profile to a row authType; an explicit auth_type wins over inference from the keys present.
func classifyProfile(p profile) Candidate {
	c := Candidate{Pair: p.pair, PairSet: p.pairSet, Env: p.env, Name: p.name, ClientID: p.clientID,
		AccountID: p.accountID, WorkspaceID: p.workspaceID,
		HasSecret: p.clientSecret != "" || p.token != ""}
	host, ok := normalizeHost(p.host)
	c.Host = host
	c.Auth, c.Skip = rowAuth(p, effectiveCLIAuth(p), ok)
	if c.Auth != providers.AuthM2M && c.Auth != providers.AuthPAT {
		c.HasSecret = false // only these two seal a secret from the file
	}
	return c
}

// effectiveCLIAuth is the explicit auth_type, else inferred from the credential keys present.
func effectiveCLIAuth(p profile) string {
	switch {
	case p.authType != "":
		return p.authType
	case p.clientID != "" && p.clientSecret != "":
		return CLIAuthM2M
	case p.token != "":
		return CLIAuthPAT
	}
	return CLIAuthU2M
}

// rowAuth maps a CLI auth type to a row auth, or the reason the profile is skipped.
func rowAuth(p profile, auth string, hostOK bool) (row, skip string) {
	switch {
	case !hostOK:
		return "", "no usable https host"
	case auth == CLIAuthU2M:
		return providers.AuthOAuth, ""
	case auth == CLIAuthM2M && p.clientID == "":
		return "", "oauth-m2m profile without client_id"
	case auth == CLIAuthM2M:
		return providers.AuthM2M, ""
	case auth == CLIAuthPAT:
		return providers.AuthPAT, ""
	}
	return "", fmt.Sprintf("unsupported auth_type %q", p.authType)
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

// Secrets are the credential values of one profile; read only on an explicit --seal-secrets.
type Secrets struct{ ClientSecret, Token string }

// ReadSecrets returns the named profile's secrets from path; DEFAULT and __settings__ are never readable.
func ReadSecrets(path, name string) (Secrets, error) {
	ps, _, err := load(path)
	if err != nil {
		return Secrets{}, err
	}
	for _, p := range ps {
		if p.name == name {
			return Secrets{ClientSecret: p.clientSecret, Token: p.token}, nil
		}
	}
	return Secrets{}, fmt.Errorf("profile %q not found", name)
}
