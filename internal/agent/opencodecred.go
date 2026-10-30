package agent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/secrets"
)

// OpencodeAPIKeyEnv is the vault key a launch passes as env when opencode holds no stored `opencode` login.
const OpencodeAPIKeyEnv = "OPENCODE_API_KEY"

// OpencodeCredID maps a model's provider prefix to the opencode credential serving it; Go and Zen share `opencode`.
func OpencodeCredID(prefix string) string {
	if prefix == "opencode-go" {
		return "opencode"
	}
	return prefix
}

// OpencodeCredential resolves prefix's credential: opencode's own stored login first, else (for `opencode`) the
// vault key as launch env; an error names what is missing. The key value rides env only, never output.
func OpencodeCredential(prefix string) (env []string, err error) {
	id := OpencodeCredID(prefix)
	if slices.Contains(OpencodeLogins(), id) {
		return nil, nil
	}
	if id == "opencode" {
		if v, _, ok := secrets.Get(OpencodeAPIKeyEnv); ok && strings.TrimSpace(v) != "" {
			return []string{OpencodeAPIKeyEnv + "=" + strings.TrimSpace(v)}, nil
		}
		return nil, fmt.Errorf("opencode has no opencode login (opencode auth login) and no %s in the vault", OpencodeAPIKeyEnv)
	}
	return nil, fmt.Errorf("opencode has no %s login (opencode auth login)", id)
}

// OpencodeOwnLoginPresent reports a usable `opencode` credential (Go/Zen plans): a stored login or the vault key.
func OpencodeOwnLoginPresent() bool {
	_, err := OpencodeCredential("opencode-go")
	return err == nil
}
