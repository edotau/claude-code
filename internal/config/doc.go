package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vendorconfig"
)

// ErrChanged reports that the vault moved between Export and Import; Import wrote nothing.
var ErrChanged = errors.New("vault changed while you edited; nothing saved")

// namePattern is the shell-identifier rule for pins and env names.
var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Doc is the editable, non-secret vault view: secrets and overlay/providers.prev never appear (D-a).
type Doc struct {
	Pins      map[string]string `json:"pins"`
	Env       map[string]string `json:"env"`
	Providers json.RawMessage   `json:"providers"`
	Vendors   json.RawMessage   `json:"vendors"`
	Settings  json.RawMessage   `json:"settings"`
	Vault     struct {
		KeyCommand string `json:"key_command"`
	} `json:"vault"`
}

// docFrom builds the doc inside an open transaction; it never opens the vault itself.
func docFrom(t *vault.Tx) (Doc, error) {
	d := Doc{Pins: t.All(vault.Pins), Env: t.All(vault.Env)}
	for name, dst := range map[string]*json.RawMessage{"providers": &d.Providers, "vendors": &d.Vendors, "settings": &d.Settings} {
		if raw, ok := t.Get(vault.Overlay, name); ok {
			if !json.Valid([]byte(raw)) {
				return Doc{}, fmt.Errorf("vault overlay/%s: invalid JSON", name)
			}
			*dst = json.RawMessage(raw)
		}
	}
	d.Vault.KeyCommand, _ = t.Get(vault.Meta, "key_command")
	return d, nil
}

// render is the canonical doc bytes Import compares against.
func render(d Doc) ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Export renders the vault's non-secret config as indented JSON.
func Export() ([]byte, error) {
	var d Doc
	err := vault.View(func(t *vault.Tx) (err error) {
		d, err = docFrom(t)
		return err
	})
	if err != nil {
		return nil, err
	}
	return render(d)
}

// Import validates edited, then compares against orig and writes the changed sections in one transaction (R11).
func Import(orig, edited []byte) error {
	var d Doc
	dec := json.NewDecoder(bytes.NewReader(edited))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return sectionError("config", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("config: trailing data")
		}
		return sectionError("config", err)
	}
	for section, values := range map[string]map[string]string{"pins": d.Pins, "env": d.Env} {
		for name, value := range values {
			if !namePattern.MatchString(name) {
				return sectionError(section+"."+strings.NewReplacer("\n", " ", "\r", " ").Replace(name), fmt.Errorf("name must match %s", namePattern))
			}
			if value == "" || strings.ContainsAny(value, "\n\r") {
				return fmt.Errorf("%s.%s: value must be non-empty and single-line", section, name)
			}
		}
	}
	d.Providers = presentJSON(d.Providers)
	d.Vendors = presentJSON(d.Vendors)
	d.Settings = presentJSON(d.Settings)
	if len(d.Providers) != 0 {
		if err := providers.Validate(d.Providers); err != nil {
			return sectionError("providers", err)
		}
	}
	if len(d.Vendors) != 0 {
		if _, err := vendorconfig.Parse(nil, d.Vendors); err != nil {
			return sectionError("vendors", err)
		}
	}
	if len(d.Settings) != 0 {
		var ov map[string]json.RawMessage
		if err := json.Unmarshal(d.Settings, &ov); err != nil {
			return sectionError("settings", err)
		}
		if err := settings.ValidateOverlay(ov); err != nil {
			return sectionError("settings", err)
		}
	}
	if d.Vault.KeyCommand != "" {
		var argv []string
		if err := json.Unmarshal([]byte(d.Vault.KeyCommand), &argv); err != nil {
			return sectionError("vault.key_command", err)
		}
		if len(argv) == 0 || argv[0] == "" {
			return errors.New("vault.key_command: must be a JSON array of strings with non-empty argv[0]")
		}
	}
	changedPins := map[string]string{}
	err := vault.Update(func(t *vault.Tx) error {
		cur, err := docFrom(t)
		if err != nil {
			return err
		}
		raw, err := render(cur)
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, orig) {
			return ErrChanged
		}
		for _, section := range []struct {
			bucket    string
			old, next map[string]string
		}{
			{vault.Pins, cur.Pins, d.Pins}, {vault.Env, cur.Env, d.Env},
		} {
			changes := map[string]string{}
			for k, v := range section.next {
				if section.old[k] != v {
					changes[k] = v
				}
			}
			for k := range section.old {
				if _, ok := section.next[k]; !ok {
					changes[k] = ""
				}
			}
			for k, v := range changes {
				if err := t.Put(section.bucket, k, v); err != nil {
					return err
				}
			}
			if section.bucket == vault.Pins {
				changedPins = changes
			}
		}
		for _, section := range []struct {
			name      string
			old, next json.RawMessage
		}{
			{"providers", cur.Providers, d.Providers}, {"vendors", cur.Vendors, d.Vendors},
			{"settings", cur.Settings, d.Settings},
		} {
			var old, next bytes.Buffer
			if raw := presentJSON(section.old); len(raw) != 0 {
				if err := json.Compact(&old, raw); err != nil {
					return err
				}
			}
			if len(section.next) != 0 {
				if err := json.Compact(&next, section.next); err != nil {
					return err
				}
			}
			if bytes.Equal(old.Bytes(), next.Bytes()) {
				continue
			}
			if section.name == "providers" {
				stored, _ := t.Get(vault.Overlay, section.name)
				if err := t.Put(vault.Overlay, "providers.prev", stored); err != nil {
					return err
				}
			}
			var stored string
			if len(section.next) != 0 {
				var indented bytes.Buffer
				if err := json.Indent(&indented, next.Bytes(), "", "  "); err != nil {
					return err
				}
				stored = indented.String() + "\n"
			}
			if err := t.Put(vault.Overlay, section.name, stored); err != nil {
				return err
			}
		}
		if cur.Vault.KeyCommand != d.Vault.KeyCommand {
			return t.Put(vault.Meta, "key_command", d.Vault.KeyCommand)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(changedPins) != 0 {
		providers.AuditPins(changedPins)
	}
	return nil
}

// presentJSON maps a JSON null section to absent.
func presentJSON(raw json.RawMessage) json.RawMessage {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return raw
}

// sectionError prefixes err with its doc section and keeps it on one line.
func sectionError(section string, err error) error {
	return fmt.Errorf("%s: %s", section, strings.NewReplacer("\n", " ", "\r", " ").Replace(err.Error()))
}

// RotateKey retains the old key so archived copies of this vault remain readable.
func RotateKey(w io.Writer, opts ...vault.RotateKeyOption) error {
	old, next, err := vault.RotateKey(opts...)
	if err != nil {
		return err
	}
	var id string
	if err := vault.View(func(t *vault.Tx) error {
		id, _ = t.Get(vault.Meta, "vault_id")
		return nil
	}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w,
		"rotated vault key %s -> %s; old key retained as vault-key/%s.%s\n",
		old, next, id, old)
	return err
}
