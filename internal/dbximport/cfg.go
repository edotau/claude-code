// Package dbximport reads a databricks CLI config ONCE and turns it into vault provider rows, so nothing
// reads ~/.databrickscfg at mint time. Enumeration returns Candidate, which has no secret field.
package dbximport

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultSection  = "DEFAULT"      // skipped on load: its values must never leak into named profiles
	settingsSection = "__settings__" // the CLI's own bookkeeping block, not a profile
)

// profile is one parsed section including secrets; it never leaves this package.
type profile struct {
	name, host, authType, clientID, clientSecret, token, accountID, workspaceID string
}

// DefaultPath is the databricks CLI's own ~/.databrickscfg.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".databrickscfg"
	}
	return filepath.Join(home, ".databrickscfg")
}

// parse scans databrickscfg text into profiles, dropping [DEFAULT] and __settings__.
func parse(r io.Reader) ([]profile, error) {
	var out []profile
	var cur *profile
	flush := func() {
		if cur != nil && !strings.EqualFold(cur.name, defaultSection) && !strings.EqualFold(cur.name, settingsSection) {
			out = append(out, *cur)
		}
	}
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		text := sc.Text()
		if first {
			text = strings.TrimPrefix(text, "\ufeff")
			first = false
		}
		line := strings.TrimSpace(text)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			flush()
			cur = &profile{name: strings.TrimSpace(line[1 : len(line)-1])}
			continue
		}
		idx := strings.IndexAny(line, "=:")
		if cur == nil || idx < 0 {
			continue
		}
		key, val := strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:])
		switch key {
		case "host":
			cur.host = val
		case "auth_type":
			cur.authType = val
		case "client_id":
			cur.clientID = val
		case "client_secret":
			cur.clientSecret = val
		case "token":
			cur.token = val
		case "account_id":
			cur.accountID = val
		case "workspace_id":
			cur.workspaceID = val
		}
	}
	flush()
	return out, sc.Err()
}

func load(path string) ([]profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(f)
}
