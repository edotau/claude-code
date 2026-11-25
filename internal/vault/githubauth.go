package vault

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// gitHubAuthKey is the Overlay entry holding the GitHub auth profile (gh's active account); "" follows gh.
const gitHubAuthKey = "github-auth"

// GitHubAuth is the stored GitHub auth profile name ("" when unset or unreadable).
func GitHubAuth() string {
	var name string
	err := View(func(tx *Tx) error {
		raw, _ := tx.Get(Overlay, gitHubAuthKey)
		var err error
		name, err = ParseGitHubAuth([]byte(raw))
		return err
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vault: cannot read %s (following gh active): %v\n", gitHubAuthKey, err)
	}
	return name
}

// ParseGitHubAuth reads a plain-text profile name, not a JSON string.
func ParseGitHubAuth(doc []byte) (string, error) {
	name := strings.TrimSpace(string(doc))
	if !utf8.Valid(doc) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", errors.New("GitHub auth profile must be UTF-8 text without control characters")
	}
	return name, nil
}

// SetGitHubAuth stores the GitHub auth profile name; "" removes it.
func SetGitHubAuth(profile string) error {
	name, err := ParseGitHubAuth([]byte(profile))
	if err != nil {
		return err
	}
	return Update(func(tx *Tx) error { return tx.Put(Overlay, gitHubAuthKey, name) })
}
