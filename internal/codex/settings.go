package codex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/render"
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
	if err := ValidateCodexOverlay(overlay); err != nil {
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
	section.WriteString("# >>> claude-code:codex-settings:begin >>>\n")
	for _, field := range CodexFields() {
		if value, ok := overlay[field.Key]; ok {
			fmt.Fprintf(&section, "%s = %q\n", field.Key, value)
		}
	}
	section.WriteString("# <<< claude-code:codex-settings:end <<<\n")
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

// codexTopLevelString strictly reads one top-level string key of config.toml; "" when it is absent.
func codexTopLevelString(raw, key string) (string, error) {
	values, err := readCodexStrings(raw, func(k string) bool { return k == key })
	return values[key], err
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
		if multiline == "" && depth == 0 {
			if err := scan.field(line); err != nil {
				return nil, err
			}
		} else if scan.pendingKey != "" {
			scan.pending += line
		}
		multiline, depth = agentconfig.TOMLLineState(line, multiline, depth)
		if scan.pendingKey != "" && multiline == "" {
			if err := scan.finish(); err != nil {
				return nil, err
			}
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

// field records a top-level "key = value" line, or starts a pending multiline string.
func (c *codexScan) field(line string) error {
	key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
	if key = agentconfig.TOMLKeyName(key); !ok || !c.want(key) {
		return nil
	}
	value = strings.TrimSpace(value)
	parsed, err := codexSettingString(value)
	switch {
	case errors.Is(err, errOpenString):
		c.pendingKey, c.pending = key, value
	case err != nil:
		return fmt.Errorf("codex: cannot read %s as a string", key)
	default:
		c.values[key] = parsed
	}
	return nil
}

// finish parses the completed multiline string.
func (c *codexScan) finish() error {
	parsed, err := codexSettingString(c.pending)
	if err != nil {
		return fmt.Errorf("codex: cannot read %s as a string", c.pendingKey)
	}
	c.values[c.pendingKey], c.pendingKey = parsed, ""
	return nil
}

var errOpenString = errors.New("unterminated multiline string")

// codexTripleString parses a TOML triple-quoted string at the start of value, trimming the first newline.
func codexTripleString(value string) (parsed, rest string, err error) {
	quote := value[:3]
	end := strings.Index(value[3:], quote)
	if end < 0 {
		return "", "", errOpenString
	}
	body := strings.TrimPrefix(strings.TrimPrefix(value[3:3+end], "\r"), "\n")
	rest = value[3+end+3:]
	if quote == "\"\"\"" {
		body = strings.ReplaceAll(strings.ReplaceAll(body, "\r", ""), "\n", `\n`)
		if body, err = strconv.Unquote(`"` + strings.ReplaceAll(body, `"`, `\"`) + `"`); err != nil {
			return "", "", err
		}
	}
	return body, rest, nil
}

func codexSettingString(value string) (string, error) {
	var parsed, rest string
	var err error
	switch {
	case strings.HasPrefix(value, "\"\"\"") || strings.HasPrefix(value, "'''"):
		parsed, rest, err = codexTripleString(value)
	case strings.HasPrefix(value, "'"):
		parsed, rest, err = codexLiteralString(value)
	default:
		parsed, rest, err = codexBasicString(value)
	}
	if err != nil {
		return "", err
	}
	if rest = strings.TrimSpace(rest); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", errors.New("unexpected text after string")
	}
	return parsed, nil
}

// codexLiteralString parses a single-quoted TOML literal string at the start of value.
func codexLiteralString(value string) (parsed, rest string, err error) {
	end := strings.IndexByte(value[1:], '\'')
	if end < 0 {
		return "", "", errors.New("unterminated literal string")
	}
	return value[1 : end+1], value[end+2:], nil
}

// codexBasicString parses a double-quoted TOML string at the start of value.
func codexBasicString(value string) (parsed, rest string, err error) {
	quoted, err := strconv.QuotedPrefix(value)
	if err != nil || !strings.HasPrefix(quoted, "\"") {
		return "", "", errors.New("expected quoted string")
	}
	if parsed, err = strconv.Unquote(quoted); err != nil {
		return "", "", err
	}
	return parsed, value[len(quoted):], nil
}

// settingsPatch locates codex's config.toml and returns its current bytes with the saved preferences applied as next.
func settingsPatch(home string) (path string, old []byte, next string, existed bool, err error) {
	if path, err = paths.CodexConfigPath(home); err != nil {
		return "", nil, "", false, err
	}
	if old, existed, err = render.ReadExisting(path); err != nil {
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
	overlay, err := LoadCodexOverlay()
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
		if err := ValidateCodexOverlay(next); err != nil {
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
	return withCodexLock(path, func() error { // write and baseline under one lock so they cannot interleave
		if err := writeCodexConfigLocked(path, old, []byte(next), existed); err != nil {
			return err
		}
		return SaveCodexBaseline(path, []byte(next))
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

// withCodexLock locks the canonical config file so every symlink to it shares one lock.
func withCodexLock(path string, fn func() error) error {
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
	return withCodexLock(path, func() error { return writeCodexConfigLocked(path, old, next, existed) })
}

// writeCodexConfigLocked is writeCodexConfig for a caller already inside withCodexLock.
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
	return withCodexLock(path, func() error { return restoreCodexConfigLocked(path, false, out) })
}

func restoreCodexConfigLocked(path string, dryRun bool, out io.Writer) error {
	backup, source, found, err := codexRestoreSource(path, dryRun)
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintf(out, "codex: no vault backup to restore for %s\n", path)
		return nil
	}
	if dryRun {
		fmt.Fprintf(out, "codex: would restore %s from %s\n", path, source)
		return nil
	}
	overlay, err := codexBackupOverlay(backup.Data)
	if err != nil {
		return err
	}
	if err := ValidateCodexOverlay(overlay); err != nil {
		return err
	}
	if err := applyCodexRestore(path, backup, overlay); err != nil {
		return err
	}
	fmt.Fprintf(out, "codex: restored %s from %s\n", path, source)
	return nil
}

// codexRestoreSource is the latest vault backup, else the legacy plaintext one (read unless dryRun); found=false when neither.
func codexRestoreSource(path string, dryRun bool) (backup CodexConfigBackup, source string, found bool, err error) {
	backup, found, err = LatestCodexBackup(path)
	if err != nil || found {
		return backup, "encrypted vault backup", found, err
	}
	src, _, ok := render.LegacyBackupSource(path)
	if !ok {
		return backup, "", false, nil
	}
	source = "legacy plaintext backup " + src
	if !dryRun {
		data, err := os.ReadFile(src)
		if err != nil {
			return backup, source, true, err
		}
		backup = CodexConfigBackup{Data: data, Existed: true}
	}
	return backup, source, true, nil
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
func applyCodexRestore(path string, backup CodexConfigBackup, overlay map[string]string) error {
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
	if err := SaveCodexBaseline(path, backup.Data); err != nil {
		return err
	}
	if backup.Name != "" { // legacy files are left in place
		return ConsumeCodexBackup(backup.Name)
	}
	return nil
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
