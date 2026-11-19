package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/httpx"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/tailscale"
)

const upstreamSchema = 1

var upstreamFormat = schemas.Register(schemas.Format{
	Name: "upstream-latest", Owner: "internal/doctor", Kind: schemas.Cache,
	Glob:    func() string { return upstreamPath() },
	New:     func() any { return &upstreamCache{} },
	Version: upstreamSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})

func upstreamPath() string { return filepath.Join(paths.StateDir(), "doctor", "upstream-latest.json") }

// upstreamTTL is the cache stamp's life: one GitHub call per key per day.
const upstreamTTL = 24 * time.Hour

type upstreamCache struct {
	Schema  int                      `json:"schema"`
	Entries map[string]upstreamEntry `json:"entries"`
}

type upstreamEntry struct {
	At    time.Time `json:"at"`
	Value string    `json:"value"`
}

// upstreamOff keeps go test off the network; tests stub fetchUpstream instead.
var upstreamOff = testing.Testing()

// fetchUpstream GETs one GitHub API path; a var for tests.
var fetchUpstream = func(ctx context.Context, path, accept string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, body, _, err := httpx.Do(ctx, httpx.Call{URL: "https://api.github.com/" + path, Limit: 1 << 20,
		Header: http.Header{"Accept": {accept}, "X-GitHub-Api-Version": {"2022-11-28"}}})
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("HTTP %d", status)
	}
	return body, err
}

// upstreamLatest is key's cached value when under a day old, else one fetched through parse and stored.
func upstreamLatest(ctx context.Context, key, path, accept string, parse func([]byte) string) (string, error) {
	var c upstreamCache
	if data, err := os.ReadFile(upstreamPath()); err == nil && !schemas.DecodeOK(upstreamFormat, data, &c) {
		c = upstreamCache{}
	}
	if e, ok := c.Entries[key]; ok && e.Value != "" && !paths.Stale(e.At, upstreamTTL, 0, time.Now()) {
		return e.Value, nil
	}
	body, err := fetchUpstream(ctx, path, accept)
	if err != nil {
		return "", err
	}
	val := parse(body)
	if val == "" {
		return "", fmt.Errorf("no value in %s", path)
	}
	if c.Entries == nil {
		c.Entries = map[string]upstreamEntry{}
	}
	c.Schema = upstreamSchema
	c.Entries[key] = upstreamEntry{At: time.Now(), Value: val}
	_ = paths.WriteJSONNoSync(upstreamPath(), c) // best effort: a failed write only costs the next call
	return val, nil
}

func releaseTag(body []byte) string {
	var r struct {
		TagName string `json:"tag_name"`
	}
	_ = json.Unmarshal(body, &r)
	return strings.TrimPrefix(r.TagName, "v")
}

// tailscaleVersionRow compares the installed client with the latest release; skipped (OK) when either is unknown.
func tailscaleVersionRow(ctx context.Context) check {
	row := check{Name: "tailscale version", OK: true}
	have, err := tailscale.ClientVersion(ctx)
	if err != nil {
		row.Detail = "skipped: client version unknown"
		return row
	}
	latest, err := upstreamLatest(ctx, "tailscale", "repos/tailscale/tailscale/releases/latest",
		"application/vnd.github+json", releaseTag)
	if err != nil {
		row.Detail = have + "; skipped: latest release unreachable"
		return row
	}
	if have == latest {
		row.Detail = have + " (latest)"
		return row
	}
	apply := "AutoUpdate.Apply unknown"
	if on, known := tailscale.AutoUpdateApply(ctx); known {
		apply = "AutoUpdate.Apply off"
		if on {
			apply = "AutoUpdate.Apply on"
		}
	}
	fix := "sudo tailscale update"
	if runtime.GOOS == "darwin" {
		fix = "app menu or auto-update"
	}
	row.OK, row.Detail = false, fmt.Sprintf("%s behind %s: %s; %s", have, latest, fix, apply)
	return row
}

// skillPin is the upstream commit SKILL.md records after "@ `".
func skillPin() string {
	data, err := os.ReadFile(filepath.Join(paths.ConfigDir(), "skills", "tailscale", "SKILL.md"))
	if err != nil {
		return ""
	}
	_, rest, ok := strings.Cut(string(data), "@ `")
	pin, _, _ := strings.Cut(rest, "`")
	if !ok || pin == "" {
		return ""
	}
	return pin
}

// tailscaleSkillRow compares the vendored skill commit with tailscale-skill's main head.
func tailscaleSkillRow(ctx context.Context) check {
	row := check{Name: "tailscale skill", OK: true}
	pin := skillPin()
	if pin == "" {
		return row
	}
	head, err := upstreamLatest(ctx, "tailscale-skill", "repos/tailscale/tailscale-skill/commits/main",
		"application/vnd.github.sha", func(b []byte) string { return strings.TrimSpace(string(b)) })
	if err != nil {
		row.Detail = pin + "; skipped: upstream unreachable"
		return row
	}
	if strings.HasPrefix(head, pin) {
		row.Detail = pin + " (upstream head)"
		return row
	}
	row.OK, row.Detail = false, fmt.Sprintf("%s behind: re-trim from upstream %.7s", pin, head)
	return row
}

// upstreamRows are the two report-only rows; none under go test unless fetchUpstream is stubbed.
func upstreamRows(ctx context.Context) []check {
	if upstreamOff {
		return nil
	}
	return []check{tailscaleVersionRow(ctx), tailscaleSkillRow(ctx)}
}
