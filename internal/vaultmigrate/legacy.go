package vaultmigrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

// docRow is one profile of the overlay's providers value; raw == nil removes it, an empty name removes the provider.
type docRow struct {
	provider, name string
	raw            json.RawMessage
}

// legacyDoc is an overlay/providers document of any past shape, read into rows and converted to the current one.
type legacyDoc struct {
	top     map[string]json.RawMessage // every key except providers
	hasRows bool                       // the document had a providers value
	rows    []docRow
	changed bool     // anything other than a dead one_m key was converted
	oneM    []string // profiles whose dead one_m key was dropped
}

// flatRowKeys mark a providers entry as one flat (pre-nesting) row rather than a provider holding profiles.
var flatRowKeys = []string{"kind", "base_url", "routes", "auth", "models", "headers", "preset", "host", "cliProfile",
	"env", "one_m", "opencodeID", "opencode_fallback", "opencode_variant", "options", "modelPrefix", "login"}

// hostProviders files a flat row that no built-in owns by the host of its first route.
var hostProviders = []struct{ suffix, group string }{
	{"anthropic.com", "anthropic"}, {"openai.com", "openai"}, {"chatgpt.com", "openai"},
	{"githubcopilot.com", "github"}, {"github.com", "github"}, {"googleapis.com", "google"},
	{"nvidia.com", "nvidia"}, {"databricks.com", providers.GroupDatabricks},
}

// readOverlay parses raw (blank = no overlay) and converts every old shape: flat rows, retired profile names,
// auth {type, authType} to auth {type, header}, the dead one_m key, and options.wire "gateway".
func readOverlay(raw string) (*legacyDoc, error) {
	d := &legacyDoc{top: map[string]json.RawMessage{}}
	if strings.TrimSpace(raw) == "" {
		return d, nil
	}
	if err := json.Unmarshal([]byte(raw), &d.top); err != nil {
		return nil, fmt.Errorf("overlay/providers: %w", err)
	}
	if d.top == nil {
		d.top = map[string]json.RawMessage{}
	}
	pv, ok := d.top["providers"]
	delete(d.top, "providers")
	if !ok {
		return d, nil
	}
	d.hasRows = true
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(pv, &entries); err != nil {
		return nil, fmt.Errorf("overlay/providers: providers: %w", err)
	}
	builtin, err := providers.ParseOverlay(nil)
	if err != nil {
		return nil, err
	}
	for _, key := range providers.ObjectKeys(pv) {
		if err := d.addEntry(key, entries[key], builtin); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (d *legacyDoc) addEntry(key string, entry json.RawMessage, builtin *providers.Registry) error {
	if bytes.Equal(bytes.TrimSpace(entry), []byte("null")) {
		if slices.Contains(providers.ProviderKeys, key) {
			d.rows = append(d.rows, docRow{provider: key})
			return nil
		}
		d.changed = true // a flat `"profile": null` removal
		name := retire(key)
		d.rows = append(d.rows, docRow{provider: builtinProvider(builtin, name), name: name})
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entry, &fields); err != nil {
		return fmt.Errorf("overlay/providers: %s: %w", key, err)
	}
	if isFlatRow(fields) {
		d.changed = true
		name := retire(key)
		prov := builtinProvider(builtin, name)
		if prov == "" {
			prov = hostProvider(fields)
		}
		raw, err := d.convertRow(name, entry)
		d.rows = append(d.rows, docRow{provider: prov, name: name, raw: raw})
		return err
	}
	for _, name := range providers.ObjectKeys(entry) {
		row := fields[name]
		if bytes.Equal(bytes.TrimSpace(row), []byte("null")) {
			d.rows = append(d.rows, docRow{provider: key, name: name})
			continue
		}
		raw, err := d.convertRow(name, row)
		if err != nil {
			return err
		}
		d.rows = append(d.rows, docRow{provider: key, name: name, raw: raw})
	}
	return nil
}

func builtinProvider(builtin *providers.Registry, name string) string {
	if p := builtin.Providers[name]; p != nil {
		return p.Provider
	}
	return ""
}

func isFlatRow(fields map[string]json.RawMessage) bool {
	return slices.ContainsFunc(flatRowKeys, func(k string) bool { _, ok := fields[k]; return ok })
}

// hostProvider is the provider of a flat row no built-in owns: databricks by preset or sealed auth, else its host.
func hostProvider(fields map[string]json.RawMessage) string {
	var row struct {
		Preset  string            `json:"preset"`
		BaseURL string            `json:"base_url"`
		Routes  map[string]string `json:"routes"`
		Auth    struct {
			AuthType string `json:"authType"`
		} `json:"auth"`
	}
	b, _ := json.Marshal(fields)
	_ = json.Unmarshal(b, &row)
	if row.Preset == "databricks" || row.Auth.AuthType != "" {
		return providers.GroupDatabricks
	}
	raw := row.BaseURL
	for _, d := range slices.Sorted(maps.Keys(row.Routes)) {
		raw = cmpOr(raw, row.Routes[d])
	}
	if u, err := url.Parse(raw); err == nil {
		for _, m := range hostProviders {
			if h := u.Hostname(); h == m.suffix || strings.HasSuffix(h, "."+m.suffix) {
				return m.group
			}
		}
	}
	return "local"
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// convertRow rewrites one profile row's old fields; it returns raw untouched when nothing in it is old.
func (d *legacyDoc) convertRow(name string, raw json.RawMessage) (json.RawMessage, error) {
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, fmt.Errorf("overlay/providers: %s: %w", name, err)
	}
	dirty := false
	if _, ok := row["one_m"]; ok {
		delete(row, "one_m")
		d.oneM = append(d.oneM, name)
		dirty = true
	}
	if a, ok := row["auth"]; ok {
		if conv, changed, err := convertAuth(a); err != nil {
			return nil, fmt.Errorf("overlay/providers: %s: auth: %w", name, err)
		} else if changed {
			row["auth"], dirty, d.changed = conv, true, true
		}
	}
	if o, ok := row["options"]; ok {
		var opts map[string]json.RawMessage
		if json.Unmarshal(o, &opts) == nil && string(opts["wire"]) == `"gateway"` {
			opts["wire"] = json.RawMessage(`"ai-gateway"`)
			row["options"], _ = json.Marshal(opts)
			dirty, d.changed = true, true
		}
	}
	if !dirty {
		return raw, nil
	}
	return json.Marshal(row)
}

// convertAuth maps the old auth {type: bearer|x-api-key|none|passthrough, authType: u2m|oauth-m2m|pat} to the
// current {type: oauth|m2m|pat|key|cli|none, header}.
func convertAuth(raw json.RawMessage) (json.RawMessage, bool, error) {
	var a map[string]json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, false, err
	}
	var typ, old string
	_ = json.Unmarshal(a["type"], &typ)
	_ = json.Unmarshal(a["authType"], &old)
	if old == "" && typ != "bearer" && typ != "x-api-key" && typ != "passthrough" {
		return raw, false, nil
	}
	header := typ
	if header != "x-api-key" {
		header = "bearer"
	}
	newType := map[string]string{"u2m": "oauth", "oauth-m2m": "m2m", "pat": "pat"}[old]
	switch {
	case newType != "":
	case typ == "passthrough":
		newType, header = "cli", ""
	case typ == "bearer" || typ == "x-api-key":
		newType = "key"
	default:
		newType, header = typ, ""
	}
	delete(a, "authType")
	a["type"], _ = json.Marshal(newType)
	if header != "" {
		a["header"], _ = json.Marshal(header)
	}
	out, err := json.Marshal(a)
	return out, true, err
}

// render writes the document in the current shape: providers nested {provider: {profile: row}} in row order.
func (d *legacyDoc) render() (string, error) {
	top := maps.Clone(d.top)
	if d.hasRows || len(d.rows) > 0 {
		var provs []string
		names := map[string][]string{}
		cells := map[string]map[string]json.RawMessage{}
		removed := map[string]bool{}
		for _, r := range d.rows {
			prov := cmpOr(r.provider, "local")
			if cells[prov] == nil {
				provs = append(provs, prov)
				cells[prov] = map[string]json.RawMessage{}
			}
			switch {
			case r.name == "":
				removed[prov] = true
			default:
				if _, dup := cells[prov][r.name]; !dup {
					names[prov] = append(names[prov], r.name)
				}
				cells[prov][r.name] = cmpRaw(r.raw)
			}
		}
		outer := map[string]json.RawMessage{}
		for _, prov := range provs {
			if removed[prov] && len(names[prov]) == 0 {
				outer[prov] = json.RawMessage("null")
				continue
			}
			b, err := providers.MarshalOrdered(names[prov], cells[prov])
			if err != nil {
				return "", err
			}
			outer[prov] = b
		}
		b, err := providers.MarshalOrdered(provs, outer)
		if err != nil {
			return "", err
		}
		top["providers"] = b
	}
	nd, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return "", err
	}
	return string(nd) + "\n", nil
}

func cmpRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return json.RawMessage("null")
	}
	return raw
}
