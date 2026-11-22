package copilotauth

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// CatalogRecord is the sealed vault record holding the latest GitHub Copilot catalog snapshot, any account.
const CatalogRecord = "copilot-catalog"

// AccountRecord is account's own snapshot record, so two GitHub login rows never evict each other's listing.
func AccountRecord(account string) string { return CatalogRecord + "/" + account }

// Snapshot is the sealed catalog of one GitHub auth (account, credential source and token digest).
type Snapshot[M any] struct {
	Schema  int
	Account string
	Source  string
	// sha256 of the token listed with; a changed token (e.g. env) must not reuse the catalog
	TokenHash    string
	DiscoveredAt time.Time
	Models       []M
}

// Model is the part of a catalog entry the id reader needs.
type Model struct {
	ID      string
	Enabled bool
}

// ReadSnapshot is the sealed snapshot for auth and tok; ok false on a miss, another account or a vault error.
func ReadSnapshot[M any](auth Auth, tok string) (Snapshot[M], bool) {
	raw, found, err := vault.ReadSecret(AccountRecord(auth.Account))
	if err != nil || !found {
		return Snapshot[M]{}, false
	}
	var s Snapshot[M]
	if json.Unmarshal([]byte(raw), &s) != nil || s.Schema != 1 || s.DiscoveredAt.IsZero() ||
		s.Account != auth.Account || s.Source != auth.Source || s.TokenHash != textutil.SHA256Hex([]byte(tok)) {
		return Snapshot[M]{}, false
	}
	return s, true
}

// CatalogToken is auth's listing token; a gh-sourced account without one errors, never another account's catalog.
func CatalogToken(auth Auth) (string, error) {
	tok, _ := ResolveToken(auth)
	if tok == "" && (auth.Source == SrcPinned || auth.Source == SrcActive) {
		return "", fmt.Errorf("copilot: no token for GitHub account %s; refusing to list another account's catalog", auth.Account)
	}
	return tok, nil
}

// CatalogAuth is the account a listing runs on: login's gh account, else the pin's; ok false = the CLI's own login.
func CatalogAuth(login string) (auth Auth, tok string, ok bool, err error) {
	if auth, ok, err = LaunchAuth(login); err != nil || !ok {
		return auth, "", false, err
	}
	tok, err = CatalogToken(auth)
	return auth, tok, true, err
}

type catalogIDs struct {
	ids []string
	at  time.Time
}

var (
	snapshotMu  sync.Mutex
	snapshotMap = paths.NewMemo[string, catalogIDs]("copilot-catalog") // per row login, read once per process
	stubIDs     func(login string) []string
	stubAt      func(login string) time.Time
)

func rowLogin(p *providers.Provider) string {
	if p == nil {
		return ""
	}
	return p.Login
}

// IDs is the enabled ids of the Copilot catalog snapshot kept for row p's account, memoized per login.
func IDs(p *providers.Provider) []string {
	snapshotMu.Lock()
	f := stubIDs
	snapshotMu.Unlock()
	if f != nil {
		return f(rowLogin(p))
	}
	return snapshotFor(p).ids
}

// Time is the same cached snapshot's timestamp, zero on a miss.
func Time(p *providers.Provider) time.Time {
	snapshotMu.Lock()
	f := stubAt
	snapshotMu.Unlock()
	if f != nil {
		return f(rowLogin(p))
	}
	return snapshotFor(p).at
}

// Reset drops the process's memo of those snapshots, so a re-list in a child process is read again.
func Reset() { snapshotMap.Drop() }

func snapshotFor(p *providers.Provider) catalogIDs {
	l := rowLogin(p)
	ids, _ := snapshotMap.Do(l, func(catalogIDs, error) time.Duration { return 0 }, func() (catalogIDs, error) {
		return readSnapshotIDs(l), nil
	})
	return ids
}

// readSnapshotIDs preserves the sealed snapshot timestamp even when no model is enabled.
func readSnapshotIDs(login string) catalogIDs {
	auth, tok, ok, err := CatalogAuth(login)
	if !ok || err != nil {
		return catalogIDs{}
	}
	s, hit := ReadSnapshot[Model](auth, tok)
	if !hit {
		return catalogIDs{}
	}
	var ids []string
	for _, m := range s.Models {
		if m.Enabled {
			ids = append(ids, m.ID)
		}
	}
	return catalogIDs{ids: ids, at: s.DiscoveredAt}
}

// StubRead replaces the ids and timestamp lookups for t (nil reads as none) and restores them on cleanup.
func StubRead(t interface {
	Helper()
	Cleanup(func())
}, ids func(login string) []string, at func(login string) time.Time) {
	t.Helper()
	snapshotMu.Lock()
	prevIDs, prevAt := stubIDs, stubAt
	stubIDs, stubAt = ids, at
	if ids == nil {
		stubIDs = func(string) []string { return nil }
	}
	if at == nil {
		stubAt = func(string) time.Time { return time.Time{} }
	}
	snapshotMu.Unlock()
	t.Cleanup(func() {
		snapshotMu.Lock()
		stubIDs, stubAt = prevIDs, prevAt
		snapshotMu.Unlock()
	})
}
