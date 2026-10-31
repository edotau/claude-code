package agent

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

// lastProfiles is state/harness/last-profile.json: the profile each workspace last launched with (a default, never a pin).
type lastProfiles struct {
	Schema     int                      `json:"schema"`
	Workspaces map[string]lastProfileAt `json:"workspaces"`
}

type lastProfileAt struct {
	Profile string    `json:"profile"`
	Updated time.Time `json:"updated"`
}

const lastProfileSchema = 1

var lastProfileFormat = schemas.Register(schemas.Format{
	Name: "last-profile", Owner: "internal/agent", Kind: schemas.Cache, Version: lastProfileSchema,
	Glob:    lastProfileFile,
	New:     func() any { return new(lastProfiles) },
	Keep:    "rebuildable cache: the per-workspace default falls back to the registry default",
	Migrate: schemas.SameShape,
})

func lastProfileFile() string { return filepath.Join(paths.StateDir(), "last-profile.json") }

// workspaceKey is the git repo root of the cwd, else the cwd.
func workspaceKey() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			return top
		}
	}
	return cwd
}

func readLastProfiles() lastProfiles {
	var st lastProfiles
	if data, err := os.ReadFile(lastProfileFile()); err == nil {
		if _, err := schemas.Decode(lastProfileFormat, data, &st); err != nil {
			st = lastProfiles{}
		}
	}
	return st
}

// lastProfile is the profile this workspace last launched, "" unless it is registered and can back Claude Code.
func lastProfile(reg *providers.Registry) string {
	name := readLastProfiles().Workspaces[workspaceKey()].Profile
	if p, err := reg.Get(name); name == "" || err != nil || p.Auth.Login != "" {
		return ""
	}
	return name
}

// RecordLastProfile stores plan's profile as this workspace's default; headless runs and failures leave it alone.
func RecordLastProfile(plan Plan, args []string) {
	name, ws := plan.Set[providers.EnvSessionProfile], workspaceKey()
	if name == "" || ws == "" || headless(args) {
		return
	}
	st := readLastProfiles()
	next := map[string]lastProfileAt{}
	maps.Copy(next, st.Workspaces)
	next[ws] = lastProfileAt{Profile: name, Updated: time.Now().UTC()}
	data, err := json.MarshalIndent(lastProfiles{Schema: lastProfileSchema, Workspaces: next}, "", "  ")
	if err == nil {
		_ = paths.AtomicWrite(lastProfileFile(), data, 0o600)
	}
}
