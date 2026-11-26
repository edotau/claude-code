// Package structdiff lists the leaf-level changes between two values of the same type, named by JSON-tag path.
package structdiff

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// Change is one differing leaf; a nil Old or New means that side is unset.
type Change struct {
	Path     string
	Old, New any
}

// Diff returns the changes from old to new sorted by path; nil on equal inputs.
func Diff(old, new any) []Change {
	o, n := map[string]any{}, map[string]any{}
	flatten(reflect.ValueOf(old), "", false, o)
	flatten(reflect.ValueOf(new), "", false, n)
	var out []Change
	for _, p := range slices.Sorted(maps.Keys(mergeKeys(o, n))) {
		if ov, nv := o[p], n[p]; !reflect.DeepEqual(ov, nv) {
			out = append(out, Change{Path: p, Old: ov, New: nv})
		}
	}
	return out
}

// Line renders "path: old → new" with (unset) for an absent side; a redacted path prints "path: (changed)".
func Line(c Change, redact func(path string) bool) string {
	if redact != nil && redact(c.Path) {
		return c.Path + ": (changed)"
	}
	return c.Path + ": " + render(c.Old) + " → " + render(c.New)
}

func render(v any) string {
	if v == nil {
		return "(unset)"
	}
	return fmt.Sprint(v)
}

func mergeKeys(a, b map[string]any) map[string]struct{} {
	keys := map[string]struct{}{}
	for _, m := range []map[string]any{a, b} {
		for k := range m {
			keys[k] = struct{}{}
		}
	}
	return keys
}

// flatten records every leaf of v under its dotted path; omit drops a zero leaf (omitempty/omitzero).
func flatten(v reflect.Value, path string, omit bool, out map[string]any) {
	switch v.Kind() {
	case reflect.Invalid:
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			flatten(v.Elem(), path, omit, out)
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
				sub = join(path, name)
			} else if !f.Anonymous {
				sub = join(path, f.Name)
			}
			flatten(v.Field(i), sub, strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero"), out)
		}
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			leaf(v, path, omit, out)
			return
		}
		for _, k := range v.MapKeys() {
			flatten(v.MapIndex(k), join(path, k.String()), false, out)
		}
	default:
		leaf(v, path, omit, out)
	}
}

func leaf(v reflect.Value, path string, omit bool, out map[string]any) {
	if omit && v.IsZero() || v.Kind() == reflect.Slice && omit && v.Len() == 0 {
		return
	}
	out[path] = v.Interface()
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
