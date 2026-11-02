package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// catalogRecord is the sealed vault record holding the GitHub Copilot catalog snapshot.
const catalogRecord = "copilot-catalog"

// catalogSnapshot is the sealed catalog of one GitHub auth (account and credential source).
type catalogSnapshot struct {
	Schema       int
	Account      string
	Source       string
	DiscoveredAt time.Time
	Models       []SDKModel
}

// readCatalogSnapshot is the sealed snapshot for auth; ok is false on a miss, another account or any vault error.
func readCatalogSnapshot(auth CopilotAuth) (catalogSnapshot, bool) {
	raw, found, err := vault.ReadSecret(catalogRecord)
	if err != nil || !found {
		return catalogSnapshot{}, false
	}
	var s catalogSnapshot
	if json.Unmarshal([]byte(raw), &s) != nil || s.Schema != 1 || s.DiscoveredAt.IsZero() || len(s.Models) == 0 ||
		s.Account != auth.Account || s.Source != auth.Source {
		return catalogSnapshot{}, false
	}
	return s, true
}

// saveCatalogSnapshot seals the snapshot under catalogRecord.
func saveCatalogSnapshot(s catalogSnapshot) error {
	data, err := json.Marshal(s)
	if err != nil {
		return errors.New("cannot encode catalog snapshot")
	}
	return vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		return tx.PutRecord(catalogRecord, vault.Record{Sealed: key.Seal(catalogRecord, string(data)), Kid: key.Kid,
			Len: len(data), Label: "github/copilot", SetAt: s.DiscoveredAt})
	})
}

// Catalog is the copilot model catalog: the sealed snapshot of the current GitHub auth with no network (an expired one
// also spawns a detached refresh), else a live listing that is sealed.
func Catalog(ctx context.Context) ([]SDKModel, error) {
	if auth, ok := copilotAuthSource(); ok {
		if s, hit := readCatalogSnapshot(auth); hit {
			if time.Since(s.DiscoveredAt) >= serving.CacheTTL() {
				if reg, err := providers.Load(); err == nil {
					if name := preflight.GithubProfile(reg); name != "" {
						preflight.SpawnDiscover(name, false)
					}
				}
			}
			return s.Models, nil
		}
	}
	return RefreshCatalog(ctx)
}

// RefreshCatalog lists the catalog live on the launch account's token and seals it, whatever the snapshot's age.
func RefreshCatalog(ctx context.Context) ([]SDKModel, error) {
	list, err := launchCatalog(ctx)
	if err != nil {
		return nil, err
	}
	auth, ok := copilotAuthSource()
	if !ok {
		return list, nil
	}
	snap := catalogSnapshot{Schema: 1, Account: auth.Account, Source: auth.Source, DiscoveredAt: time.Now().UTC(), Models: list}
	if err := saveCatalogSnapshot(snap); err != nil {
		fmt.Fprintf(os.Stderr, "copilot: catalog snapshot not saved: %v\n", err)
	}
	return list, nil
}
