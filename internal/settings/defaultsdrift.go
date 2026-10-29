package settings

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/paths"
)

// DefaultsDrift lists top-level keys and "env.X" keys whose live value differs from RenderLive, sorted.
func DefaultsDrift() (keys []string, err error) {
	live, err := os.ReadFile(filepath.Join(paths.ConfigDir(), "settings.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rendered, err := RenderLive()
	if err != nil {
		return nil, err
	}
	return DiffRendered(live, rendered)
}

// DiffRendered lists the top-level and "env.X" keys where existing differs from what a restore writes
// (rendered): changed values, keys only in existing, keys only in rendered; sorted. Runtime keys are ignored.
func DiffRendered(existing, rendered []byte) (keys []string, err error) {
	lo, err := parseObject(bytes.TrimPrefix(existing, []byte("\xef\xbb\xbf")))
	if err != nil {
		return nil, fmt.Errorf("existing settings: %w", ErrInvalidJSON)
	}
	ro, err := parseObject(rendered)
	if err != nil {
		return nil, err
	}
	keys, err = diffObjects(lo, ro, "", func(k string) bool { return k == "env" || slices.Contains(runtimeKeys, k) })
	if err != nil {
		return nil, err
	}
	le, err := parseObject(orEmpty(lo.vals["env"]))
	if err != nil {
		return nil, fmt.Errorf("settings.json env: %w", err)
	}
	re, err := parseObject(orEmpty(ro.vals["env"]))
	if err != nil {
		return nil, err
	}
	envKeys, err := diffObjects(le, re, "env.", func(string) bool { return false })
	if err != nil {
		return nil, err
	}
	keys = append(keys, envKeys...)
	slices.Sort(keys)
	return keys, nil
}

// diffObjects lists the keys (prefixed) present in only one object or holding different values, skipping skip(k).
func diffObjects(a, b *object, prefix string, skip func(string) bool) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, k := range append(slices.Clone(a.keys), b.keys...) {
		if seen[k] || skip(k) {
			continue
		}
		seen[k] = true
		av, aok := a.vals[k]
		bv, bok := b.vals[k]
		if aok != bok {
			out = append(out, prefix+k)
			continue
		}
		ac, err := compactStr(av)
		if err != nil {
			return nil, fmt.Errorf("%s%s: %w", prefix, k, err)
		}
		bc, err := compactStr(bv)
		if err != nil {
			return nil, fmt.Errorf("%s%s: %w", prefix, k, err)
		}
		if ac != bc {
			out = append(out, prefix+k)
		}
	}
	return out, nil
}
