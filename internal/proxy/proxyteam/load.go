package proxyteam

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/schemas"
)

const schemaVersion = 1

var overlayFormat = schemas.Register(schemas.Format{
	Name: "proxy-teams", Owner: "internal/proxy/proxyteam", Kind: schemas.Wire, Version: schemaVersion,
	New: func() any { return new(Config) },
})

var lastGoodFormat = schemas.Register(schemas.Format{
	Name: "proxy-teams-last-good", Owner: "internal/proxy/proxyteam", Kind: schemas.Cache, Version: schemaVersion,
	Glob: lastGoodPath,
	New:  func() any { return new(Config) },
	Keep: "rebuildable cache; absent means the embedded default",
})

func lastGoodPath() string { return filepath.Join(paths.StateDir(), "proxy", "teams.last-good.json") }

// decode reads one schema document strictly: a stale or newer version and an unknown field are errors.
func decode(f schemas.Format, raw []byte) (Config, error) {
	var c Config
	if _, err := schemas.Decode(f, raw, &c); err != nil {
		return Config{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(new(Config)); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Parse merges overlay into def (a team of the same name is replaced, any other is added) and validates the result.
func Parse(def, overlay []byte) (Config, []Problem) {
	c, err := decode(overlayFormat, def)
	if err != nil {
		return Config{}, []Problem{{"", "default", err.Error()}}
	}
	if len(bytes.TrimSpace(overlay)) == 0 {
		return c, Validate(c)
	}
	o, err := decode(overlayFormat, overlay)
	if err != nil {
		return c, []Problem{{"", "overlay", err.Error() + overlayHint}}
	}
	n, replaced := len(c.Teams), map[string]bool{}
	for _, t := range o.Teams {
		i := slices.IndexFunc(c.Teams[:n], func(d Team) bool { return d.Name == t.Name })
		if i >= 0 && !replaced[t.Name] {
			c.Teams[i], replaced[t.Name] = t, true
		} else {
			c.Teams = append(c.Teams, t)
		}
	}
	return c, markOverlay(Validate(c), o.Teams)
}

// overlayHint names where an overlay problem comes from and how to clear it.
const overlayHint = " (vault overlay proxy-teams; remove: claude-code vault rm overlay proxy-teams --yes)"

// markOverlay tags each problem on an overlay team, or on the config as a whole, with its source.
func markOverlay(ps []Problem, overlay []Team) []Problem {
	for i, p := range ps {
		if p.Team == "" || slices.ContainsFunc(overlay, func(t Team) bool { return t.Name == p.Team }) {
			ps[i].Detail += overlayHint
		}
	}
	return ps
}

// Load merges overlay into the embedded default; on a problem it returns the last good config, else the default.
func Load(overlay []byte) (Config, []Problem) {
	def := assets.ProxyTeams()
	c, ps := Parse(def, overlay)
	if len(ps) == 0 {
		saveLastGood(c)
		return c, nil
	}
	if raw, err := os.ReadFile(lastGoodPath()); err == nil {
		if g, err := decode(lastGoodFormat, raw); err == nil && len(Validate(g)) == 0 {
			return g, ps
		}
	}
	d, dps := Parse(def, nil)
	return d, append(ps, dps...)
}

func saveLastGood(c Config) {
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	// a failed save only costs the next bad overlay its last-good, so it is not an error
	_, _ = paths.WriteIfChanged(lastGoodPath(), append(b, '\n'), 0o600)
}
