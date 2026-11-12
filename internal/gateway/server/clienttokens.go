package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// Client tokens are long-lived, named, vault-held (hash only) tokens for an editor; accepted on serving-row /p/ routes.
const (
	clientTokenPf     = "cck_"
	clientTokenSecret = "HARNESS_PROXY_CLIENT_"
)

var clientTokenName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// clientSecretName is the vault name holding name's token hash; name is 1-32 of [a-z0-9-], lower-cased.
func clientSecretName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !clientTokenName.MatchString(name) {
		return "", fmt.Errorf("client token name %q: want 1-32 of a-z, 0-9, - (starting alphanumeric)", name)
	}
	return clientTokenSecret + strings.ToUpper(strings.ReplaceAll(name, "-", "_")), nil
}

// MintClientToken seals a new token's SHA-256 under name (replacing any earlier one) and returns the token.
func MintClientToken(name string) (string, error) {
	secret, err := clientSecretName(name)
	if err != nil {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := clientTokenPf + hex.EncodeToString(b)
	if err := vault.Store(secret, textutil.SHA256Hex([]byte(tok))); err != nil {
		return "", fmt.Errorf("store client token %s: %w", name, err)
	}
	return tok, nil
}

// RevokeClientToken deletes name's token hash; existed is false when no such token was minted.
func RevokeClientToken(name string) (existed bool, err error) {
	secret, err := clientSecretName(name)
	if err != nil {
		return false, err
	}
	names, err := vault.NamesErr()
	if err != nil {
		return false, err
	}
	if !slices.Contains(names, secret) {
		return false, nil
	}
	return true, vault.Delete(secret)
}

// clientTokens caches the vault's client token hashes, reloaded whenever the vault file changes.
type clientTokens struct {
	mu     sync.Mutex
	stamp  string // vault mtime+size the hashes were read at; "" = not loaded
	hashes []string
	load   func() ([]string, error) // a seam for tests
}

// loadClientHashes reads every client token hash sealed in the vault.
func loadClientHashes() ([]string, error) {
	names, err := vault.NamesErr()
	if err != nil {
		return nil, err
	}
	var want []string
	for _, n := range names {
		if strings.HasPrefix(n, clientTokenSecret) {
			want = append(want, n)
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	vals, err := vault.ReadSecrets(want)
	return slices.Collect(maps.Values(vals)), err
}

// current is the cached hashes, reloaded on a vault change; a failed read fails closed and retries next call.
func (c *clientTokens) current() []string {
	stamp := "missing"
	if fi, err := os.Stat(vault.Path()); err == nil {
		stamp = fmt.Sprintf("%d/%d", fi.ModTime().UnixNano(), fi.Size())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if stamp == c.stamp {
		return c.hashes
	}
	hashes, err := c.load()
	if err != nil {
		c.stamp, c.hashes = "", nil
		return nil
	}
	c.stamp, c.hashes = stamp, hashes
	return hashes
}

// valid reports whether tok is a minted, unrevoked client token, comparing every hash in constant time.
func (c *clientTokens) valid(tok string) bool {
	if !strings.HasPrefix(tok, clientTokenPf) {
		return false
	}
	hash, ok := []byte(textutil.SHA256Hex([]byte(tok))), false
	for _, h := range c.current() {
		if subtle.ConstantTimeCompare([]byte(h), hash) == 1 {
			ok = true
		}
	}
	return ok
}
