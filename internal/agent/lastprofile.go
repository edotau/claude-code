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
	Name: "last-profile", Owner: "internal/agent", Kind: schemas.Cache, Version: lastProfileSchema,
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

// lastProfile is the profile this workspace last launched agentName with, "" unless it is registered and the agent can use it.
func lastProfile(reg *providers.Registry, agentName string) string {
	name := readLastProfiles().Workspaces[workspaceKey()][agentName].Profile
	p, err := reg.Get(name)
	if name == "" || err != nil {
		return ""
	}
	if agentName == "claude" && p.Auth.Login != "" || agentName != "claude" && !CanServe(agentName, p) {
		return ""
	}
	return name
}

// withLastProfile fills an unset --profile of an agent CLI launch from this workspace's last-used profile (before its pin).
func withLastProfile(name string, o LaunchOptions) LaunchOptions {
	if o.Provider != "" || AgentPins()[name].Off || (name != "copilot" && name != "opencode") {
		return o
	}
	if reg, err := providers.Load(); err == nil {
		o.Provider = lastProfile(reg, name)
	}
	return o
}

// ClearLastProfiles forgets every workspace's default, so a fresh `switch` pin applies everywhere.
func ClearLastProfiles() error {
	if err := os.Remove(lastProfileFile()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ClearLastProfilesFor forgets the defaults of every agent whose profile pin is a key of pins, in all workspaces.
func ClearLastProfilesFor(pins map[string]string) error {
	var gone []string
	for _, k := range rosterRows {
		if _, ok := pins[k.CLI.ProfilePin]; ok && k.CLI.ProfilePin != "" {
			gone = append(gone, k.Name)
		}
	}
	if len(gone) == 0 {
		return nil
	}
	st := readLastProfiles()
	next := map[string]map[string]lastProfileAt{}
	for ws, byAgent := range st.Workspaces {
		kept := maps.Clone(byAgent)
		for _, n := range gone {
			delete(kept, n)
		}
		if len(kept) > 0 {
			next[ws] = kept
		}
	}
	return writeLastProfiles(next)
}

func writeLastProfiles(ws map[string]map[string]lastProfileAt) error {
	data, err := json.MarshalIndent(lastProfiles{Schema: lastProfileSchema, Workspaces: ws}, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(lastProfileFile(), data, 0o600)
}

// RecordLastProfile stores profile as this workspace's default for agentName; headless runs leave it alone.
func RecordLastProfile(agentName, profile string, args []string) {
	ws := workspaceKey()
	if profile == "" || ws == "" || headless(args) {
		return
	}
	next := map[string]map[string]lastProfileAt{}
	for k, v := range readLastProfiles().Workspaces {
		next[k] = maps.Clone(v)
	}
	if next[ws] == nil {
		next[ws] = map[string]lastProfileAt{}
	}
	next[ws][agentName] = lastProfileAt{Profile: profile, Updated: time.Now().UTC()}
	_ = writeLastProfiles(next)
}
