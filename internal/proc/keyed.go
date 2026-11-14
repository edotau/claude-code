package proc

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
)

// Values holds keyed string values until a child-tool boundary needs a wire shape.
type Values map[string]string

// Env returns entries sorted by name.
func (v Values) Env() []string {
	out := make([]string, 0, len(v))
	for _, name := range slices.Sorted(maps.Keys(v)) {
		out = append(out, name+"="+v[name])
	}
	return out
}

// ValuesFromEnv parses entries, keeping the last value for each nonempty name.
func ValuesFromEnv(env []string) Values {
	out := make(Values)
	for _, entry := range env {
		if name, value, ok := strings.Cut(entry, "="); ok && name != "" {
			out[name] = value
		}
	}
	return out
}

// With returns a copy with name set to value.
func (v Values) With(name, value string) Values {
	return v.Merge(Values{name: value})
}

// Merge returns a copy with over taking precedence.
func (v Values) Merge(over Values) Values {
	out := make(Values, len(v)+len(over))
	maps.Copy(out, v)
	maps.Copy(out, over)
	return out
}

// JSON returns a flat object with sorted keys, including an empty object for nil.
func (v Values) JSON() ([]byte, error) {
	return json.Marshal(v.Merge(nil))
}

// Args returns repeated flag and key=value pairs sorted by key.
func (v Values) Args(flag string) []string {
	out := make([]string, 0, 2*len(v))
	for _, entry := range v.Env() {
		out = append(out, flag, entry)
	}
	return out
}

type valuesKey struct{}

// WithValues stores a complete environment snapshot, copied from values.
func WithValues(ctx context.Context, values Values) context.Context {
	return context.WithValue(ctx, valuesKey{}, values.Merge(nil))
}

// ValuesFromContext returns a copy of the snapshot and whether one was installed.
func ValuesFromContext(ctx context.Context) (Values, bool) {
	values, ok := ctx.Value(valuesKey{}).(Values)
	if !ok {
		return nil, false
	}
	return values.Merge(nil), true
}

// Value reads the complete run snapshot, falling back to the environment only when no snapshot exists.
func Value(ctx context.Context, name string) string {
	if values, ok := ctx.Value(valuesKey{}).(Values); ok {
		return values[name]
	}
	return os.Getenv(name)
}
