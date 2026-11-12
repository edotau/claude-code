package codex

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

const codexSettingsBegin = "# >>> claude-code:codex-settings:begin >>>"
const codexSettingsEnd = "# <<< claude-code:codex-settings:end <<<"

// codexSettingsSection finds our root banner without matching text inside multiline values or tables.
func codexSettingsSection(raw string) (start, end int, err error) {
	start, end = -1, -1
	offset, depth, multiline := 0, 0, ""
	for _, line := range strings.SplitAfter(raw, "\n") {
		if multiline == "" && depth == 0 {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "[") {
				break
			}
			if trimmed == codexSettingsBegin {
				if start >= 0 {
					return -1, -1, errors.New("codex: duplicate settings banner")
				}
				start = offset
			}
			if trimmed == codexSettingsEnd && start >= 0 {
				return start, offset + len(line), nil
			}
		}
		multiline, depth = agentconfig.TOMLLineState(line, multiline, depth)
		offset += len(line)
	}
	if start >= 0 {
		return -1, -1, errors.New("codex: incomplete settings banner")
	}
	return start, end, nil
}

func patchCodexSettings(raw string, overlay map[string]string) (string, error) {
	if err := validateCodexOverlay(overlay); err != nil {
		return "", err
	}
	start, end, err := codexSettingsSection(raw)
	if err != nil {
		return "", err
	}
	base := raw
	if start >= 0 {
		// The insertion owns one separator newline; remove it so table-free files stay idempotent.
		if strings.HasSuffix(raw[:start], "\n\n") {
			start--
		}
		base = raw[:start] + raw[end:]
	}
	if len(overlay) == 0 {
		return base, nil
	}
	var section strings.Builder
	section.WriteString(codexSettingsBegin + "\n")
	for _, field := range CodexFields() {
		if value, ok := overlay[field.Key]; ok {
			fmt.Fprintf(&section, "%s = %q\n", field.Key, value)
		}
	}
	section.WriteString(codexSettingsEnd + "\n")
	return agentconfig.InsertTopLevelTOML(base, section.String()), nil
}

// ReadCodexSettings returns only explicit, supported native preferences; no credentials or unknown keys.
func ReadCodexSettings(home string) (map[string]string, error) {
	path, err := paths.CodexConfigPath(home)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return readCodexSettings(string(raw))
}

func readCodexSettings(raw string) (map[string]string, error) {
	return readCodexStrings(raw, func(key string) bool {
		return slices.ContainsFunc(CodexFields(), func(f CodexField) bool { return f.Key == key })
	})
}

// readCodexStrings parses the top-level string values of the keys want accepts.
func readCodexStrings(raw string, want func(string) bool) (map[string]string, error) {
	scan := &codexScan{values: map[string]string{}, want: want}
	lines := strings.SplitAfter(raw, "\n")
	if index := agentconfig.FirstTopLevelHeaderIndex(lines); index >= 0 {
		lines = lines[:index]
	}
	multiline, depth := "", 0
	for _, line := range lines {
		var err error
		multiline, depth, err = scan.line(line, multiline, depth)
		if err != nil {
			return nil, err
		}
	}
	if scan.pendingKey != "" {
		return nil, fmt.Errorf("codex: cannot read %s: unterminated multiline string", scan.pendingKey)
	}
	return scan.values, nil
}

// codexScan collects string preferences; pendingKey/pending hold a multiline string still being read.
type codexScan struct {
	values              map[string]string
	pendingKey, pending string
	want                func(string) bool
}

func (c *codexScan) line(line, multiline string, depth int) (string, int, error) {
	if multiline == "" && depth == 0 {
		if err := c.field(line); err != nil {
			return multiline, depth, err
		}
	} else if c.pendingKey != "" {
		c.pending += line
	}
	multiline, depth = agentconfig.TOMLLineState(line, multiline, depth)
	if c.pendingKey != "" && multiline == "" {
		return multiline, depth, c.finish()
	}
	return multiline, depth, nil
}

// field records a top-level "key = value" line, or starts a pending multiline string.
func (c *codexScan) field(line string) error {
	key, raw, ok := strings.Cut(line, "=")
	if key = agentconfig.TOMLKeyName(key); !ok || !c.want(key) {
		return nil
	}
	value := strings.TrimSpace(raw)
	parsed, err := agentconfig.TOMLStringValue(value)
	switch {
	case errors.Is(err, agentconfig.ErrOpenString):
		c.pendingKey, c.pending = key, strings.TrimLeft(raw, " \t") // keeps the line's newline
	case err != nil:
		return fmt.Errorf("codex: cannot read %s as a string", key)
	default:
		c.values[key] = parsed
	}
	return nil
}

// finish parses the completed multiline string.
func (c *codexScan) finish() error {
	parsed, err := agentconfig.TOMLStringValue(c.pending)
	if err != nil {
		return fmt.Errorf("codex: cannot read %s as a string", c.pendingKey)
	}
	c.values[c.pendingKey], c.pendingKey = parsed, ""
	return nil
}

// settingsPatch locates codex's config.toml and returns its current bytes with the saved preferences applied as next.
func settingsPatch(home string) (path string, old []byte, next string, existed bool, err error) {
	if path, err = paths.CodexConfigPath(home); err != nil {
		return "", nil, "", false, err
	}
	if old, existed, err = agentconfig.ReadExisting(path); err != nil {
		return "", nil, "", false, err
	}
	prefs, err := LoadCodexPreferences()
	if err != nil {
		return "", nil, "", false, err
	}
	next, err = patchCodexSettings(string(old), prefs)
	return path, old, next, existed, err
}

// codexEffortKey is config.toml's effort key; the record keeps the effort in its own field.
const codexEffortKey = "model_reasoning_effort"

// codexPreferences is the legacy codex-settings overlay with the record's effort and model_verbosity laid over it.
func codexPreferences(rec vault.AgentRecord, overlay map[string]string) (map[string]string, error) {
	prefs := maps.Clone(overlay)
	if prefs == nil {
		prefs = map[string]string{}
	}
	if rec.Effort != "" {
		native, _ := agentid.NativeEffort(agentid.Codex, agentid.Effort(rec.Effort))
		if native == "" {
			return nil, fmt.Errorf("codex: record %w", agentid.ValidEffort(agentid.Codex, rec.Effort))
		}
		prefs[codexEffortKey] = native
	}
	if v := rec.Extras[extraVerbosity]; v != "" {
		prefs[extraVerbosity] = v
	}
	return prefs, nil
}

// LoadCodexPreferences is the config.toml preferences: the codex record over the legacy codex-settings overlay.
func LoadCodexPreferences() (map[string]string, error) {
	rec, err := agentconfig.Settings(agentid.Codex)
	if err != nil {
		return nil, err
	}
	overlay, err := readCodexOverlay()
	if err != nil {
		return nil, err
	}
	return codexPreferences(rec, overlay)
}

// UpdateCodexPreferences edits the preferences in one transaction, writing them to the codex record and emptying
// the legacy overlay; an effort whose codex spelling is unchanged keeps the record's own value (max stays max).
func UpdateCodexPreferences(edit func(map[string]string) map[string]string) error {
	return vault.Update(func(tx *vault.Tx) error {
		overlay, err := loadCodexOverlay(tx)
		if err != nil {
			return err
		}
		recs, err := tx.AgentRecords()
		if err != nil {
			return err
		}
		rec := recs[agentid.Codex]
		prefs, err := codexPreferences(rec, overlay)
		if err != nil {
			return err
		}
		next := edit(prefs)
		if err := validateCodexOverlay(next); err != nil {
			return err
		}
		if recEffort, _ := agentid.NativeEffort(agentid.Codex, agentid.Effort(rec.Effort)); next[codexEffortKey] != recEffort {
			rec.Effort = next[codexEffortKey]
		}
		rec.Extras = withExtra(rec.Extras, extraVerbosity, next[extraVerbosity])
		recs[agentid.Codex] = rec
		if err := tx.PutAgentRecords(recs); err != nil {
			return err
		}
		return tx.Put(vault.Overlay, codexOverlayKey, "")
	})
}

// withExtra is extras with key set to v, or removed when v is "".
func withExtra(extras map[string]string, key, v string) map[string]string {
	out := maps.Clone(extras)
	if v == "" {
		delete(out, key)
		if len(out) == 0 {
			return nil
		}
		return out
	}
	if out == nil {
		out = map[string]string{}
	}
	out[key] = v
	return out
}

// CodexSettingsDrift includes stale managed keys whose override was removed from the vault.
func CodexSettingsDrift(home string) (bool, error) {
	_, old, next, _, err := settingsPatch(home)
	return !bytes.Equal(old, []byte(next)), err
}

// ApplyCodexSettings applies only saved preferences, leaving provider generation and other agents alone.
func ApplyCodexSettings(home string) error {
	path, old, next, existed, err := settingsPatch(home)
	if err != nil || bytes.Equal(old, []byte(next)) {
		return err
	}
	if _, err := readCodexSettings(next); err != nil {
		return err
	}
	return WithConfigLock(path, func() error { // write and baseline under one lock so they cannot interleave
		if err := writeCodexConfigLocked(path, old, []byte(next), existed); err != nil {
			return err
		}
		return saveCodexBaseline(path, []byte(next))
	})
}

func codexWriteTarget(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return filepath.EvalSymlinks(path)
	}
	return path, nil
}

// WithConfigLock locks the canonical config file so every symlink to it shares one lock.
func WithConfigLock(path string, fn func() error) error {
	canon, err := codexWriteTarget(path)
	if errors.Is(err, os.ErrNotExist) {
		canon, err = path, nil // dangling link: lock the link path itself
	}
	if err != nil {
		return err
	}
	return paths.WithFileLock(canon, func() error {
		if now, err := codexWriteTarget(path); (err == nil && now != canon) || (err != nil && canon != path) {
			return errors.New("codex: config symlink changed before lock; retry")
		}
		return fn()
	})
}

func writeCodexConfig(path string, old, next []byte, existed bool) error {
	return WithConfigLock(path, func() error { return writeCodexConfigLocked(path, old, next, existed) })
}

// writeCodexConfigLocked is writeCodexConfig for a caller already inside WithConfigLock.
func writeCodexConfigLocked(path string, old, next []byte, existed bool) error {
	target, err := codexWriteTarget(path)
	if err != nil {
		return err
	}
	check := func() error { return checkCodexUnchanged(path, target, old, existed) }
	if err := check(); err != nil {
		return err
	}
	if err := CodexBackup(path, old, existed); err != nil {
		return fmt.Errorf("codex: encrypted backup failed: %w", err)
	}
	if err := check(); err != nil {
		return err
	}
	return paths.AtomicWrite(target, next, 0o600)
}

// checkCodexUnchanged fails when the config's content or symlink target moved since it was read.
func checkCodexUnchanged(path, target string, old []byte, existed bool) error {
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if (err == nil) != existed || !bytes.Equal(current, old) {
		return errors.New("codex: config changed before write; retry to preserve the newer settings")
	}
	now, err := codexWriteTarget(path)
	if err != nil {
		return err
	}
	if now != target {
		return errors.New("codex: config symlink changed before write; retry")
	}
	return nil
}

func restoreCodexConfig(path string, dryRun bool, out io.Writer) error {
	if dryRun {
		return restoreCodexConfigLocked(path, true, out)
	}
	return WithConfigLock(path, func() error { return restoreCodexConfigLocked(path, false, out) })
}

func restoreCodexConfigLocked(path string, dryRun bool, out io.Writer) error {
	backup, found, err := LatestCodexBackup(path)
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintf(out, "codex: no vault backup to restore for %s\n", path)
		return nil
	}
	source := "encrypted vault backup"
	if dryRun {
		fmt.Fprintf(out, "codex: would restore %s from %s\n", path, source)
		return nil
	}
	overlay, err := codexBackupOverlay(backup.Data)
	if err != nil {
		return err
	}
	if err := validateCodexOverlay(overlay); err != nil {
		return err
	}
	if err := applyCodexRestore(path, backup, overlay); err != nil {
		return err
	}
	fmt.Fprintf(out, "codex: restored %s from %s\n", path, source)
	return nil
}

// codexBackupOverlay is the preferences recorded in a backup's managed settings section.
func codexBackupOverlay(data []byte) (map[string]string, error) {
	start, end, err := codexSettingsSection(string(data))
	if err != nil {
		return nil, err
	}
	if start < 0 {
		return map[string]string{}, nil
	}
	return readCodexSettings(string(data[start:end]))
}

// applyCodexRestore puts the backup back on disk and resynchronizes the overlay, baseline and backup list.
func applyCodexRestore(path string, backup codexConfigBackup, overlay map[string]string) error {
	target, err := codexWriteTarget(path)
	if err != nil {
		return err
	}
	if backup.Existed {
		if err := paths.AtomicWrite(target, backup.Data, 0o600); err != nil {
			return err
		}
	} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := UpdateCodexPreferences(func(map[string]string) map[string]string { return overlay }); err != nil {
		return fmt.Errorf("codex: file restored but preferences could not be synchronized: %w", err)
	}
	if err := saveCodexBaseline(path, backup.Data); err != nil {
		return err
	}
	return consumeCodexBackup(backup.Name)
}

var codexManagedPaths = func() []string {
	if runtime.GOOS == "windows" {
		root := os.Getenv("ProgramData")
		if root == "" {
			root = `C:\ProgramData`
		}
		return []string{filepath.Join(root, "OpenAI", "Codex", "requirements.toml")}
	}
	return []string{"/etc/codex/managed_config.toml", "/etc/codex/requirements.toml"}
}

// CodexConfigChecks reports configuration sources without reading or printing their contents.
func CodexConfigChecks() []agent.Check {
	var checks []agent.Check
	for _, path := range codexManagedPaths() {
		checks = append(checks, codexConfigCheck("codex machine config", path, true))
	}
	cfg, err := paths.CodexConfigPath("")
	if err != nil {
		return append(checks, agent.Check{Name: "codex user config", State: "warn", Detail: "home unavailable; effective route unknown"})
	}
	return append(checks, codexConfigCheck("codex user config", cfg, false))
}

func codexConfigCheck(name, path string, managed bool) agent.Check {
	row := agent.Check{Name: name, State: "ok", Detail: path + ": absent"}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return row
	}
	if err != nil || !info.Mode().IsRegular() {
		row.State, row.Detail = "warn", path+": unavailable; effective route unknown"
		return row
	}
	if managed {
		row.Detail = path + ": present; machine policy may override CLI settings; effective route unverified"
	} else {
		row.Detail = path + ": present; CLI and machine settings may override it"
	}
	return row
}

const codexOverlayKey = "codex-settings"

type CodexField struct {
	Key    string
	Values []string
}

func CodexFields() []CodexField {
	return []CodexField{
		{"model_reasoning_effort", agentid.NativeEfforts(agentid.Codex)},
		{extraVerbosity, codexVerbosities},
	}
}

func validateCodexOverlay(ov map[string]string) error {
	for key, value := range ov {
		found := false
		for _, f := range CodexFields() {
			if f.Key == key {
				found = true
				if !slices.Contains(f.Values, value) {
					return fmt.Errorf("codex: invalid value for %s; choose one of: %s", key, strings.Join(f.Values, ", "))
				}
				break
			}
		}
		if !found {
			return fmt.Errorf("codex: unsupported preference %s; use model_reasoning_effort or model_verbosity", key)
		}
	}
	return nil
}

func loadCodexOverlay(tx *vault.Tx) (map[string]string, error) {
	ov := map[string]string{}
	if raw, ok := tx.Get(vault.Overlay, codexOverlayKey); ok {
		if err := json.Unmarshal([]byte(raw), &ov); err != nil {
			return nil, fmt.Errorf("codex: invalid saved preferences: %w", err)
		}
	}
	if ov == nil {
		ov = map[string]string{}
	}
	return ov, validateCodexOverlay(ov)
}

func readCodexOverlay() (ov map[string]string, err error) {
	err = vault.View(func(tx *vault.Tx) error { ov, err = loadCodexOverlay(tx); return err })
	return
}

type codexConfigBackup struct {
	Name    string `json:"-"`
	Path    string
	Data    []byte
	Existed bool
	At      time.Time
}

func codexPathID(path string) (string, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(abs))
	return abs, hex.EncodeToString(sum[:]), nil
}

func putCodexSealed(tx *vault.Tx, key *vault.Key, name string, data []byte) error {
	return tx.PutRecordIn(vault.SettingsBackups, name, vault.Record{
		Sealed: key.SealSettingsBackup(name, string(data)), Kid: key.Kid, SetAt: time.Now().UTC(),
	})
}

func CodexBackup(path string, raw []byte, existed bool) error {
	abs, id, err := codexPathID(path)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	prefix := "codex/" + id + "/"
	name := prefix + now.Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(nonce[:])
	data, err := json.Marshal(codexConfigBackup{Path: abs, Data: raw, Existed: existed, At: now})
	if err != nil {
		return err
	}
	return vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		if err := putCodexSealed(tx, key, name, data); err != nil {
			return err
		}
		var names []string
		for _, n := range tx.Names(vault.SettingsBackups) {
			if strings.HasPrefix(n, prefix) {
				names = append(names, n)
			}
		}
		for _, n := range names[:max(0, len(names)-5)] {
			if err := tx.Put(vault.SettingsBackups, n, ""); err != nil {
				return err
			}
		}
		return nil
	})
}

func LatestCodexBackup(path string) (codexConfigBackup, bool, error) {
	abs, id, err := codexPathID(path)
	if err != nil {
		return codexConfigBackup{}, false, err
	}
	prefix, name := "codex/"+id+"/", ""
	err = vault.View(func(tx *vault.Tx) error {
		for _, n := range tx.Names(vault.SettingsBackups) {
			if strings.HasPrefix(n, prefix) {
				name = n
			}
		}
		return nil
	})
	if err != nil || name == "" {
		return codexConfigBackup{}, false, err
	}
	raw, found, err := vault.ReadSettingsBackup(name)
	if err != nil || !found {
		return codexConfigBackup{}, false, err
	}
	var b codexConfigBackup
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return b, false, fmt.Errorf("codex: invalid encrypted backup: %w", err)
	}
	if b.Path != abs {
		return b, false, errors.New("codex: backup path mismatch")
	}
	b.Name = name
	return b, true, nil
}

func consumeCodexBackup(name string) error {
	if !strings.HasPrefix(name, "codex/") {
		return errors.New("codex: invalid backup name")
	}
	return vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.SettingsBackups, name, "") })
}

func saveCodexBaseline(path string, raw []byte) error {
	_, id, err := codexPathID(path)
	if err != nil {
		return err
	}
	return vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error { return putCodexSealed(tx, key, "codex-baseline/"+id, raw) })
}

func loadCodexBaseline(path string) ([]byte, error) {
	_, id, err := codexPathID(path)
	if err != nil {
		return nil, err
	}
	raw, found, err := vault.ReadSettingsBackup("codex-baseline/" + id)
	if err != nil || !found {
		return nil, err
	}
	return []byte(raw), nil
}
