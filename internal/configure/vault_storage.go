package configure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// ErrChanged reports that the vault moved between Export and Import; Import wrote nothing.
var ErrChanged = errors.New("vault changed while you edited; nothing saved")

// namePattern is the shell-identifier rule for pins and env names.
var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Doc is the editable, non-secret vault view: secrets and overlay/providers.prev never appear (D-a).
// Agents is the agents overlay plus the agent records under "records".
type Doc struct {
	Pins      map[string]string `json:"pins"`
	Env       map[string]string `json:"env"`
	Providers json.RawMessage   `json:"providers"`
	Agents    json.RawMessage   `json:"agents"`
	Settings  json.RawMessage   `json:"settings"`
	Vault     struct {
		KeyCommand string `json:"key_command"`
	} `json:"vault"`
}

// docFrom builds the doc inside an open transaction; it never opens the vault itself.
func docFrom(t *vault.Tx) (Doc, error) {
	d := Doc{Pins: t.All(vault.Pins), Env: t.All(vault.Env)}
	for name, dst := range map[string]*json.RawMessage{"providers": &d.Providers, "agents": &d.Agents, "settings": &d.Settings} {
		if raw, ok := t.Get(vault.Overlay, name); ok {
			if !json.Valid([]byte(raw)) {
				return Doc{}, fmt.Errorf("vault overlay/%s: invalid JSON", name)
			}
			*dst = json.RawMessage(raw)
		}
	}
	recs, err := t.AgentRecords()
	if err != nil {
		return Doc{}, err
	}
	if d.Agents, err = mergeAgentRecords(d.Agents, recs); err != nil {
		return Doc{}, err
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

// decodeDoc strictly decodes edited as a single Doc value, rejecting unknown fields and trailing data.
func decodeDoc(edited []byte) (Doc, error) {
	var d Doc
	dec := json.NewDecoder(bytes.NewReader(edited))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Doc{}, sectionError("config", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Doc{}, errors.New("config: trailing data")
		}
		return Doc{}, sectionError("config", err)
	}
	return d, nil
}

// validatePinsEnv enforces the shell-identifier name rule and single-line, non-empty values.
func validatePinsEnv(d Doc) error {
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
	return nil
}

// validateOverlaySections normalizes null overlay sections to absent and validates each one present.
func validateOverlaySections(d *Doc) error {
	d.Providers = presentJSON(d.Providers)
	d.Agents = presentJSON(d.Agents)
	d.Settings = presentJSON(d.Settings)
	if len(d.Providers) != 0 {
		if err := providers.Validate(d.Providers); err != nil {
			return sectionError("providers", err)
		}
	}
	if len(d.Agents) != 0 {
		if _, err := agent.ParseConfig(nil, d.Agents); err != nil {
			return sectionError("agents", err)
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
	return nil
}

// applyPinsEnv diffs cur against d for the pins and env buckets and writes the changes; it returns the pin diff.
func applyPinsEnv(t *vault.Tx, cur, d Doc) (map[string]string, error) {
	changedPins := map[string]string{}
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
				return nil, err
			}
		}
		if section.bucket == vault.Pins {
			changedPins = changes
		}
	}
	return changedPins, nil
}

// applyOverlaySections writes each changed providers/agents/settings overlay section, backing up providers first.
func applyOverlaySections(t *vault.Tx, cur, d Doc) error {
	for _, section := range []struct {
		name      string
		old, next json.RawMessage
	}{
		{"providers", cur.Providers, d.Providers}, {"agents", cur.Agents, d.Agents},
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
	return nil
}

// Import validates edited, then compares against orig and writes the changed sections in one transaction (R11).
func Import(orig, edited []byte) error {
	d, err := decodeDoc(edited)
	if err != nil {
		return err
	}
	if err := validatePinsEnv(d); err != nil {
		return err
	}
	var recs map[string]vault.AgentRecord
	if d.Agents, recs, err = splitAgentRecords(d.Agents); err != nil {
		return err
	}
	if err := validateOverlaySections(&d); err != nil {
		return err
	}
	var changedPins map[string]string
	err = vault.Update(func(t *vault.Tx) error {
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
		var curRecs map[string]vault.AgentRecord
		if cur.Agents, curRecs, err = splitAgentRecords(cur.Agents); err != nil {
			return err
		}
		if err := applyAgentRecords(t, curRecs, recs); err != nil {
			return err
		}
		changedPins, err = applyPinsEnv(t, cur, d)
		if err != nil {
			return err
		}
		if err := applyOverlaySections(t, cur, d); err != nil {
			return err
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

// isTerminal and confirmEdit are seams for the re-edit prompt.
var isTerminal = tui.StdinIsTerminal
var confirmEdit = func(q string) bool { return tui.ConfirmKey(q, true) }

// Edit opens the non-secret config in $VISUAL/$EDITOR via a 0600 temp file and imports it; the file is always removed.
func Edit(stdout, stderr io.Writer) error {
	orig, err := Export()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "claude-code-config-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }() // best-effort cleanup of the temp file
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(orig); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	argv := strings.Fields(os.Getenv("VISUAL"))
	if len(argv) == 0 {
		argv = strings.Fields(os.Getenv("EDITOR"))
	}
	if len(argv) == 0 {
		argv = []string{"vi"}
		if runtime.GOOS == "windows" {
			argv[0] = "notepad"
		}
	}
	for {
		cmd := exec.CommandContext(context.Background(), argv[0], append(argv[1:], f.Name())...) // top-level: interactive editor, no caller ctx
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		swallow := make(chan os.Signal, 1) // caught, not ignored: SIG_IGN would stick and be inherited by later children
		signal.Notify(swallow, os.Interrupt)
		err := cmd.Run()
		signal.Stop(swallow)
		if err != nil {
			return fmt.Errorf("editor %s: %w", argv[0], err)
		}
		edited, err := os.ReadFile(f.Name())
		if err != nil {
			return err
		}
		if bytes.Equal(edited, orig) {
			fmt.Fprintln(stdout, "no changes")
			return nil
		}
		err = Import(orig, edited)
		if err == nil {
			fmt.Fprintln(stdout, "saved")
			return nil
		}
		if errors.Is(err, ErrChanged) {
			return err
		}
		if !isTerminal() {
			return err
		}
		fmt.Fprintln(stderr, "config:", err)
		if !confirmEdit("Re-edit?") {
			return err
		}
	}
}
