package vaultdoc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"runtime"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// errChanged reports that the vault moved between Export and importDoc; importDoc wrote nothing.
var errChanged = errors.New("vault changed while you edited; nothing saved")

// Doc is the editable, non-secret vault view (no secrets, no providers.prev); Agents carries "records" (D-a).
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

// render is the canonical doc bytes importDoc compares against.
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

// docSchema is the newest doc shape this build reads; Export does not stamp it, a hand-added "schema" is checked.
const docSchema = 1

// docKeys are the top-level keys importDoc reads; "$schema" is an editor hint, any other key is reported and ignored.
var docKeys = []string{"schema", "$schema", "pins", "env", "providers", "agents", "settings", "vault"}

// parsed is an edited doc decoded entry by entry: what applies, the paths that keep their stored value, the warnings.
type parsed struct {
	doc      Doc
	recs     map[string]vault.AgentRecord
	keep     map[string]bool // "pins", "pins.NAME", "agents.records.codex", "vault", ...
	warnings []string
}

// skip keeps path's stored value and records why the edited one was not imported; values never appear.
func (p *parsed) skip(path string, err error) {
	p.keep[path] = true
	p.warnings = append(p.warnings, fmt.Sprintf("unable to parse %s: %s", oneLine(path), oneLine(err.Error())))
}

// decodeTop decodes edited as one JSON object, rejecting non-objects (null would wipe config) and trailing data.
func decodeTop(edited []byte) (map[string]json.RawMessage, error) {
	if t := bytes.TrimSpace(edited); len(t) == 0 || t[0] != '{' {
		return nil, sectionError("config", errors.New("must be a JSON object"))
	}
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(edited))
	if err := dec.Decode(&top); err != nil {
		return nil, syntaxError(edited, err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, errors.New("config: trailing data")
		}
		return nil, syntaxError(edited, err)
	}
	return top, nil
}

// syntaxError names the line and column of a JSON syntax error; other decode errors keep their own text.
func syntaxError(doc []byte, err error) error {
	se, ok := errors.AsType[*json.SyntaxError](err)
	if !ok {
		return sectionError("config", err)
	}
	before := doc[:min(int(se.Offset), len(doc))]
	line, col := bytes.Count(before, []byte("\n"))+1, len(before)-1-bytes.LastIndexByte(before, '\n')
	return sectionError("config", fmt.Errorf("invalid JSON at line %d, column %d: %w", line, col, se))
}

// parseDoc decodes each section and entry on its own, so one bad entry is reported instead of failing the whole doc.
func parseDoc(top map[string]json.RawMessage) (*parsed, error) {
	p := &parsed{keep: map[string]bool{}}
	if err := p.schema(top["schema"]); err != nil {
		return nil, err
	}
	for _, k := range slices.Sorted(maps.Keys(top)) {
		if !slices.Contains(docKeys, k) {
			p.warnings = append(p.warnings, fmt.Sprintf("unable to parse %s: unknown field, ignored", oneLine(k)))
		}
	}
	p.doc.Pins = p.stringMap("pins", top["pins"])
	p.doc.Env = p.stringMap("env", top["env"])
	p.vaultSection(top["vault"])
	p.overlays(top)
	return p, nil
}

// schema accepts a doc stamped at or below docSchema and refuses a newer one, which this build cannot read.
func (p *parsed) schema(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		p.skip("schema", err)
		return nil
	}
	if n > docSchema {
		return fmt.Errorf("config: schema %d is newer than this build reads (%d); nothing saved", n, docSchema)
	}
	return nil
}

// stringMap decodes a pins/env section entry by entry: a bad name or value keeps that entry's stored value.
func (p *parsed) stringMap(section string, raw json.RawMessage) map[string]string {
	var entries map[string]json.RawMessage
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &entries); err != nil {
			p.skip(section, err)
			return nil
		}
	}
	out := make(map[string]string, len(entries))
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		var v string
		err := json.Unmarshal(entries[name], &v)
		switch {
		case err != nil:
		case !vault.EnvName.MatchString(name):
			err = fmt.Errorf("name must match %s", vault.EnvName)
		case v == "" || strings.ContainsAny(v, "\n\r"):
			err = errors.New("value must be non-empty and single-line")
		}
		if err != nil {
			p.skip(section+"."+name, err)
			continue
		}
		out[name] = v
	}
	return out
}

// vaultSection decodes the vault section strictly; a bad one keeps the stored key_command.
func (p *parsed) vaultSection(raw json.RawMessage) {
	if len(presentJSON(raw)) == 0 {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p.doc.Vault); err != nil {
		p.skip("vault", err)
	} else if err := validateKeyCommand(p.doc.Vault.KeyCommand); err != nil {
		p.skip("vault.key_command", err)
	}
}

// overlays splits agent records out of agents, decodes them per agent, and validates each overlay section whole.
func (p *parsed) overlays(top map[string]json.RawMessage) {
	if agents, recs, err := cutRecords(top["agents"]); err != nil {
		p.skip("agents", err)
		p.keep["agents."+recordsKey] = true
	} else {
		p.recs = p.agentRecords(recs)
		p.doc.Agents = p.overlay("agents", agents)
	}
	p.doc.Providers = p.overlay("providers", top["providers"])
	p.doc.Settings = p.overlay("settings", top["settings"])
}

// overlay returns the present section when it validates; an invalid one keeps the stored section.
func (p *parsed) overlay(name string, raw json.RawMessage) json.RawMessage {
	raw = presentJSON(raw)
	if err := validateOverlay(name, raw); err != nil {
		p.skip(name, err)
		return nil
	}
	return raw
}

// validateOverlay checks one present overlay section; settings is retired, so any content would save with no effect.
func validateOverlay(name string, raw json.RawMessage) error {
	switch {
	case len(raw) == 0:
		return nil
	case name == "providers":
		return providers.Validate(raw)
	case name == "agents":
		_, err := agentconfig.ParseConfig(raw)
		return err
	}
	return errors.New("retired overlay has no effect; delete the section")
}

// validateKeyCommand checks vault.key_command, when set, is a JSON argv array with a non-empty argv[0].
func validateKeyCommand(keyCommand string) error {
	if keyCommand == "" {
		return nil
	}
	var argv []string
	if err := json.Unmarshal([]byte(keyCommand), &argv); err != nil {
		return err
	}
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("must be a JSON array of strings with non-empty argv[0]")
	}
	return nil
}

// applyPinsEnv diffs cur against d for the pins and env buckets and writes the changes.
func applyPinsEnv(t *vault.Tx, cur, d Doc) error {
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
	}
	return nil
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
		if err := applyOverlaySection(t, section.name, section.old, section.next); err != nil {
			return err
		}
	}
	return nil
}

// compactJSON compacts raw, or returns nil for an absent section.
func compactJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// applyOverlaySection writes one overlay section when its compacted form changed; providers keep a prev copy.
func applyOverlaySection(t *vault.Tx, name string, oldRaw, nextRaw json.RawMessage) error {
	old, err := compactJSON(presentJSON(oldRaw))
	if err != nil {
		return err
	}
	next, err := compactJSON(nextRaw)
	if err != nil {
		return err
	}
	if bytes.Equal(old, next) {
		return nil
	}
	if name == "providers" {
		stored, _ := t.Get(vault.Overlay, name)
		if err := t.Put(vault.Overlay, "providers.prev", stored); err != nil {
			return err
		}
	}
	var stored string
	if len(nextRaw) != 0 {
		var indented bytes.Buffer
		if err := json.Indent(&indented, next, "", "  "); err != nil {
			return err
		}
		stored = indented.String() + "\n"
	}
	return t.Put(vault.Overlay, name, stored)
}

// importDoc applies each entry that parses (R11: one tx, vault equal to orig), warning per bad one; none applied errs.
func importDoc(orig, edited []byte) (warnings []string, err error) {
	top, err := decodeTop(edited)
	if err != nil {
		return nil, err
	}
	p, err := parseDoc(top)
	if err != nil {
		return nil, err
	}
	base, baseRecs, err := origDoc(orig)
	if err != nil {
		return nil, err
	}
	p.refuseInvalidRecords(baseRecs)
	d, recs := p.merge(base, baseRecs)
	if len(p.warnings) > 0 && !docChanged(base, d, baseRecs, recs) {
		return nil, fmt.Errorf("nothing saved:\n  %s", strings.Join(p.warnings, "\n  "))
	}
	var curRecs map[string]vault.AgentRecord
	err = vault.Update(func(t *vault.Tx) error {
		var txErr error
		curRecs, txErr = importTx(t, orig, d, recs)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	providers.AuditRecords(curRecs, recs)
	return p.warnings, nil
}

// origDoc decodes the exported doc orig with its records split out; importTx proves the vault still equals it.
func origDoc(orig []byte) (Doc, map[string]vault.AgentRecord, error) {
	var d Doc
	if err := json.Unmarshal(orig, &d); err != nil {
		return Doc{}, nil, sectionError("original config", err)
	}
	d.Providers, d.Settings = presentJSON(d.Providers), presentJSON(d.Settings)
	agents, recs, err := splitAgentRecords(d.Agents)
	d.Agents = agents
	return d, recs, err
}

// merge is p's doc with every kept section or entry restored from base, the stored config.
func (p *parsed) merge(base Doc, baseRecs map[string]vault.AgentRecord) (Doc, map[string]vault.AgentRecord) {
	d := p.doc
	d.Pins, d.Env = p.keptMap("pins", base.Pins, d.Pins), p.keptMap("env", base.Env, d.Env)
	if p.keep["providers"] {
		d.Providers = base.Providers
	}
	if p.keep["agents"] {
		d.Agents = base.Agents
	}
	if p.keep["settings"] {
		d.Settings = base.Settings
	}
	if p.keep["vault"] || p.keep["vault.key_command"] {
		d.Vault = base.Vault
	}
	return d, keptRecords(p, baseRecs)
}

// keptMap is next with each kept entry of section restored from base; a kept section is base whole.
func (p *parsed) keptMap(section string, base, next map[string]string) map[string]string {
	if p.keep[section] {
		return base
	}
	out := maps.Clone(next)
	for k, v := range base {
		if p.keep[section+"."+k] {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

// docChanged reports whether applying next over old would write anything, by the apply functions' own comparisons.
func docChanged(old, next Doc, oldRecs, nextRecs map[string]vault.AgentRecord) bool {
	if !maps.Equal(old.Pins, next.Pins) || !maps.Equal(old.Env, next.Env) || old.Vault != next.Vault {
		return true
	}
	if (len(oldRecs) != 0 || len(nextRecs) != 0) && !reflect.DeepEqual(oldRecs, nextRecs) {
		return true
	}
	return !sameJSON(old.Providers, next.Providers) || !sameJSON(old.Agents, next.Agents) || !sameJSON(old.Settings, next.Settings)
}

// sameJSON reports whether two overlay sections compact to the same bytes (null and absent are the same).
func sameJSON(a, b json.RawMessage) bool {
	ca, errA := compactJSON(presentJSON(a))
	cb, errB := compactJSON(presentJSON(b))
	return errA == nil && errB == nil && bytes.Equal(ca, cb)
}

// importTx writes d when the stored config still equals orig and returns the records stored before the write.
func importTx(t *vault.Tx, orig []byte, d Doc, recs map[string]vault.AgentRecord) (curRecs map[string]vault.AgentRecord, err error) {
	cur, err := docFrom(t)
	if err != nil {
		return nil, err
	}
	raw, err := render(cur)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, orig) {
		return nil, errChanged
	}
	if cur.Agents, curRecs, err = splitAgentRecords(cur.Agents); err != nil {
		return curRecs, err
	}
	if err := applyAgentRecords(t, curRecs, recs); err != nil {
		return curRecs, err
	}
	if err := applyPinsEnv(t, cur, d); err != nil {
		return curRecs, err
	}
	if err := applyOverlaySections(t, cur, d); err != nil {
		return curRecs, err
	}
	if cur.Vault.KeyCommand != d.Vault.KeyCommand {
		return curRecs, t.Put(vault.Meta, "key_command", d.Vault.KeyCommand)
	}
	return curRecs, nil
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
	return fmt.Errorf("%s: %s", section, oneLine(err.Error()))
}

// oneLine folds line breaks to spaces so a key or message cannot forge an extra output line.
func oneLine(s string) string { return strings.NewReplacer("\n", " ", "\r", " ").Replace(s) }

// RotateKey retains the old key so archived copies of this vault remain readable.
func RotateKey(w io.Writer, force bool) error {
	old, next, err := vault.RotateKey(force)
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
	// Migrate first: importDoc's write migrates, so an unmigrated export would always read as "vault changed".
	if err := vault.Update(func(*vault.Tx) error { return nil }); err != nil {
		return err
	}
	orig, err := Export()
	if err != nil {
		return err
	}
	name, err := writeEditorScratch(orig)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(name) }() // best-effort cleanup of the temp file
	argv := editorArgv()
	for {
		if err := runEditor(argv, name); err != nil {
			return err
		}
		edited, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if bytes.Equal(edited, orig) {
			fmt.Fprintln(stdout, "no changes")
			return nil
		}
		retry, err := importEdited(stdout, stderr, orig, edited)
		if !retry {
			return err
		}
	}
}

// writeEditorScratch writes orig to a 0600 temp file and returns its name; the caller removes it.
func writeEditorScratch(orig []byte) (string, error) {
	f, err := os.CreateTemp("", "claude-code-config-*.json")
	if err != nil {
		return "", err
	}
	err = f.Chmod(0600)
	if err == nil {
		_, err = f.Write(orig)
	}
	if err = errors.Join(err, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// editorArgv is $VISUAL, else $EDITOR, else the platform's default editor.
func editorArgv() []string {
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
	return argv
}

// runEditor runs the editor on file with the terminal attached, swallowing SIGINT meant for the editor.
func runEditor(argv []string, file string) error {
	cmd := exec.CommandContext(context.Background(), argv[0], append(argv[1:], file)...) // top-level: interactive editor, no caller ctx
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	swallow := make(chan os.Signal, 1) // caught, not ignored: SIG_IGN would stick and be inherited by later children
	signal.Notify(swallow, os.Interrupt)
	err := cmd.Run()
	signal.Stop(swallow)
	if err != nil {
		return fmt.Errorf("editor %s: %w", argv[0], err)
	}
	return nil
}

// importEdited imports the edited doc; retry is true only when the import failed and the user chose to re-edit.
func importEdited(stdout, stderr io.Writer, orig, edited []byte) (retry bool, err error) {
	warnings, err := importDoc(orig, edited)
	if err == nil {
		for _, w := range warnings {
			fmt.Fprintln(stderr, "warning:", w)
		}
		fmt.Fprintln(stdout, "saved")
		return false, nil
	}
	if errors.Is(err, errChanged) || !isTerminal() {
		return false, err
	}
	fmt.Fprintln(stderr, "config:", err)
	if !confirmEdit("Re-edit?") {
		return false, err
	}
	return true, nil
}
