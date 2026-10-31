package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/paths"
)

// ErrInvalidJSON wraps a settings file that failed to parse.
var ErrInvalidJSON = errors.New("not valid JSON")

// Diff reports how existing differs from what Merge(existing, rendered) would write.
func Diff(existing, rendered []byte) (differs, foreign []string, err error) {
	if !json.Valid(existing) {
		return nil, nil, fmt.Errorf("existing settings: %w", ErrInvalidJSON)
	}
	merged, sum, err := Merge(existing, rendered, nil)
	if err != nil {
		return nil, nil, err
	}
	ex, err := parseObject(existing)
	if err != nil {
		return nil, nil, err
	}
	mg, err := parseObject(merged)
	if err != nil {
		return nil, nil, err
	}
	tp, err := parseObject(rendered)
	if err != nil {
		return nil, nil, err
	}
	for _, k := range tp.keys {
		exCompact, err := compactStr(ex.vals[k])
		if err != nil {
			return nil, nil, fmt.Errorf("existing %s: %w", k, err)
		}
		mgCompact, err := compactStr(mg.vals[k])
		if err != nil {
			return nil, nil, fmt.Errorf("merged %s: %w", k, err)
		}
		if exCompact != mgCompact {
			differs = append(differs, k)
		}
	}
	return differs, sum.Kept, nil
}

// LocalDuplicates lists settings.local.json keys (top level and env.X) whose value equals the rendered one.
func LocalDuplicates(local, rendered []byte) ([]string, error) {
	lo, err := parseObject(orEmpty(local))
	if err != nil {
		return nil, fmt.Errorf("local settings: %w", err)
	}
	rd, err := parseObject(rendered)
	if err != nil {
		return nil, err
	}
	var dups []string
	for _, k := range lo.keys {
		if k == "env" {
			continue
		}
		same, err := sameValue(lo.vals[k], rd.vals, k)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		if same {
			dups = append(dups, k)
		}
	}
	lEnv, err := parseObject(orEmpty(lo.vals["env"]))
	if err != nil {
		return nil, fmt.Errorf("local env: %w", err)
	}
	rEnv, err := parseObject(orEmpty(rd.vals["env"]))
	if err != nil {
		return nil, fmt.Errorf("rendered env: %w", err)
	}
	for _, k := range lEnv.keys {
		same, err := sameValue(lEnv.vals[k], rEnv.vals, k)
		if err != nil {
			return nil, fmt.Errorf("env.%s: %w", k, err)
		}
		if same {
			dups = append(dups, "env."+k)
		}
	}
	return dups, nil
}

// sameValue reports whether local's value for k (already looked up) compacts equal to rendered's.
func sameValue(localVal json.RawMessage, rendered map[string]json.RawMessage, k string) (bool, error) {
	renderedVal, ok := rendered[k]
	if !ok {
		return false, nil
	}
	lc, err := compactStr(localVal)
	if err != nil {
		return false, err
	}
	rc, err := compactStr(renderedVal)
	if err != nil {
		return false, err
	}
	return lc == rc, nil
}

// compactStr compacts raw JSON for comparison; a missing value (nil) compacts to "".
func compactStr(raw json.RawMessage) (string, error) {
	if raw == nil {
		return "", nil
	}
	// Decode and re-encode so escape-equivalent spellings (\u003c vs <) compare equal.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

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
