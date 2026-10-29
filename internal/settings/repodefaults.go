package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/paths"
)

// RepoDefaultsRel is the tracked repo defaults file, relative to the config dir.
const RepoDefaultsRel = "defaults/claude/settings.json"

// RepoDefaultsFile is the path of the repo defaults file in the live config dir.
func RepoDefaultsFile() string {
	return filepath.Join(paths.ConfigDir(), filepath.FromSlash(RepoDefaultsRel))
}

// WriteRepoDefaults merges staged into the repo defaults file (env variables go under env), keeping
// $schema first and 2-space JSON; it returns the path written.
func WriteRepoDefaults(staged map[string]json.RawMessage) (string, error) {
	path := RepoDefaultsFile()
	cur, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	o, err := parseObject(cur)
	if err != nil {
		return "", fmt.Errorf("%s: %w", RepoDefaultsRel, err)
	}
	for _, k := range slices.Sorted(maps.Keys(staged)) {
		if err := validateKey(k, staged[k]); err != nil {
			return "", fmt.Errorf("%s: %w", k, err)
		}
		if err := setKey(o, k, staged[k]); err != nil {
			return "", err
		}
	}
	if i := slices.Index(o.keys, "$schema"); i > 0 {
		o.keys = slices.Concat([]string{"$schema"}, o.keys[:i], o.keys[i+1:])
	}
	out, err := o.marshal()
	if err != nil {
		return "", err
	}
	if bytes.Equal(out, cur) {
		return path, nil
	}
	return path, os.WriteFile(path, out, 0o644)
}
