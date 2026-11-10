package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
)

// lastProfiles is state/harness/last-profile.json: the profile each workspace last launched each agent with (a default, never a pin).
type lastProfiles struct {
	Schema     int                                 `json:"schema"`
	Workspaces map[string]map[string]lastProfileAt `json:"workspaces"`
}

type lastProfileAt struct {
	Profile string    `json:"profile"`
	Updated time.Time `json:"updated"`
}

const lastProfileSchema = 2

var lastProfileFormat = schemas.Register(schemas.Format{
	Name: "last-profile", Owner: "internal/providers", Kind: schemas.Cache, Version: lastProfileSchema,
	Glob:    lastProfileFile,
	New:     func() any { return new(lastProfiles) },
	Keep:    "rebuildable cache: the per-workspace default falls back to the registry default",
	Migrate: migrateLastProfile,
})

// migrateLastProfile steps v0 -> v1 (stamp) and v1 -> v2 (each workspace entry becomes its "claude" entry).
func migrateLastProfile(from int, raw []byte) ([]byte, error) {
	if from != 1 {
		return schemas.SameShape(from, raw)
	}
	var v1 struct {
		Workspaces map[string]lastProfileAt `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &v1); err != nil {
		return nil, err
	}
	next := lastProfiles{Schema: 2, Workspaces: map[string]map[string]lastProfileAt{}}
	for ws, at := range v1.Workspaces {
		next.Workspaces[ws] = map[string]lastProfileAt{"claude": at}
	}
	return json.Marshal(next)
}

func lastProfileFile() string { return filepath.Join(paths.StateDir(), "last-profile.json") }

// workspaceKeys memoizes workspaceKey per cwd, so a launch spawns git at most once per directory.
var workspaceKeys sync.Map

// workspaceKey is the git repo root of the cwd, else the cwd.
func workspaceKey() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if k, ok := workspaceKeys.Load(cwd); ok {
		return k.(string)
	}
	k := gitTopOr(cwd)
	workspaceKeys.Store(cwd, k)
	return k
}

// gitTopOr is cwd's git repo root, else cwd.
func gitTopOr(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := proc.RunGit(ctx, cwd, "rev-parse", "--show-toplevel"); err == nil && strings.TrimSpace(out) != "" {
		return strings.TrimSpace(out)
	}
	return cwd
}

// loadLastProfiles is the stored state plus any decode error; a corrupt file is copied to .corrupt-<unix>, a newer one refused.
func loadLastProfiles() (lastProfiles, error) {
	var st lastProfiles
	path := lastProfileFile()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return lastProfiles{}, err
	}
	if _, err := schemas.Decode(lastProfileFormat, data, &st); err != nil {
		if errors.Is(err, schemas.ErrNewer) {
			return lastProfiles{}, err
		}
		if berr := paths.AtomicWrite(fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix()), data, 0o600); berr != nil {
			return lastProfiles{}, errors.Join(err, berr)
		}
		return lastProfiles{}, nil
	}
	return st, nil
}

// updateLastProfiles applies fn to the stored workspaces under the file lock and writes them back.
func updateLastProfiles(fn func(map[string]map[string]lastProfileAt) map[string]map[string]lastProfileAt) error {
	return paths.WithFileLock(lastProfileFile(), func() error {
		st, err := loadLastProfiles()
		if err != nil {
			return err
		}
		return writeLastProfiles(fn(st.Workspaces))
	})
}

// ClearLastProfiles forgets every workspace's default, so a fresh `switch` pin applies everywhere.
func ClearLastProfiles() error {
	return paths.WithFileLock(lastProfileFile(), func() error {
		if err := os.Remove(lastProfileFile()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}

// ClearLastProfilesFor forgets the defaults of the named agents, in all workspaces.
func ClearLastProfilesFor(gone ...string) error {
	if len(gone) == 0 {
		return nil
	}
	return updateLastProfiles(func(cur map[string]map[string]lastProfileAt) map[string]map[string]lastProfileAt {
		next := map[string]map[string]lastProfileAt{}
		for ws, byAgent := range cur {
			kept := maps.Clone(byAgent)
			for _, n := range gone {
				delete(kept, n)
			}
			if len(kept) > 0 {
				next[ws] = kept
			}
		}
		return next
	})
}

func writeLastProfiles(ws map[string]map[string]lastProfileAt) error {
	data, err := json.MarshalIndent(lastProfiles{Schema: lastProfileSchema, Workspaces: ws}, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(lastProfileFile(), data, 0o600)
}

// LastProfile is the profile this workspace last launched agentName with; "" when none is stored.
func LastProfile(agentName string) string {
	st, err := loadLastProfiles()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: last profile for %s unreadable: %v\n", agentName, err)
	}
	return st.Workspaces[workspaceKey()][agentName].Profile
}

// RecordLastProfile stores profile as this workspace's default for agentName; callers skip headless runs.
func RecordLastProfile(agentName, profile string) {
	ws := workspaceKey()
	if profile == "" || ws == "" {
		return
	}
	err := updateLastProfiles(func(cur map[string]map[string]lastProfileAt) map[string]map[string]lastProfileAt {
		next := map[string]map[string]lastProfileAt{}
		for k, v := range cur {
			next[k] = maps.Clone(v)
		}
		if next[ws] == nil {
			next[ws] = map[string]lastProfileAt{}
		}
		next[ws][agentName] = lastProfileAt{Profile: profile, Updated: time.Now().UTC()}
		return next
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: last profile for %s not saved: %v\n", agentName, err)
	}
}
