// structdiff lists the leaf-level changes between two values of the same type, named by JSON-tag path.
package providers

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// Change is one differing leaf; a nil Old or New means that side is unset.
type structChange struct {
	Path     string
	Old, New any
}

// Diff returns the changes from old to new sorted by path; nil on equal inputs.
func diffStructs(old, new any) []structChange {
	o, n := map[string]any{}, map[string]any{}
	flattenStruct(reflect.ValueOf(old), "", false, o)
	flattenStruct(reflect.ValueOf(new), "", false, n)
	var out []structChange
	for _, p := range sortedMapKeys(o, n) {
		if ov, nv := o[p], n[p]; !reflect.DeepEqual(ov, nv) {
			out = append(out, structChange{Path: p, Old: ov, New: nv})
		}
	}
	return out
}

// Line renders "path: old → new" with (unset) for an absent side; a redacted path prints "path: (changed)".
func diffLine(c structChange, redact func(path string) bool) string {
	if redact != nil && redact(c.Path) {
		return c.Path + ": (changed)"
	}
	return c.Path + ": " + renderDiffVal(c.Old) + " → " + renderDiffVal(c.New)
}

func renderDiffVal(v any) string {
	if v == nil {
		return "(unset)"
	}
	return fmt.Sprint(v)
}

// SortedKeys is the sorted union of a's and b's keys.
func sortedMapKeys[V any](a, b map[string]V) []string {
	keys := map[string]struct{}{}
	for _, m := range []map[string]V{a, b} {
		for k := range m {
			keys[k] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(keys))
}

// flatten records every leaf of v under its dotted path; omit drops a zero leaf (omitempty/omitzero).
func flattenStruct(v reflect.Value, path string, omit bool, out map[string]any) {
	switch v.Kind() {
	case reflect.Invalid:
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			flattenStruct(v.Elem(), path, omit, out)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.IsExported() || name == "-" && opts == "" {
				continue
			}
			sub := path
			if name != "" {
				sub = joinDiffPath(path, name)
			} else if !f.Anonymous {
				sub = joinDiffPath(path, f.Name)
			}
			flattenStruct(v.Field(i), sub, strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero"), out)
		}
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			leafDiff(v, path, omit, out)
			return
		}
		for _, k := range v.MapKeys() {
			flattenStruct(v.MapIndex(k), joinDiffPath(path, k.String()), false, out)
		}
	default:
		leafDiff(v, path, omit, out)
	}
}

func leafDiff(v reflect.Value, path string, omit bool, out map[string]any) {
	if omit && (v.IsZero() || v.Kind() == reflect.Slice && v.Len() == 0) {
		return
	}
	out[path] = v.Interface()
}

func joinDiffPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
