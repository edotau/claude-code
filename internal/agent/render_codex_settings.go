package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
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
		multiline, depth = tomlLineState(line, multiline, depth)
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
	return insertTopLevelTOML(base, section.String()), nil
}

// ReadCodexSettings returns only explicit, supported native preferences; no credentials or unknown keys.
func ReadCodexSettings(home string) (map[string]string, error) {
	path, err := CodexConfigPath(home)
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
	values := map[string]string{}
	pendingKey, pending := "", ""
	lines := strings.SplitAfter(raw, "\n")
	if index := firstTopLevelHeaderIndex(lines); index >= 0 {
		lines = lines[:index]
	}
	multiline, depth := "", 0
	for _, line := range lines {
		if multiline == "" && depth == 0 {
			if key, value, ok := codexFieldLine(line); ok {
				parsed, err := codexSettingString(value)
				if errors.Is(err, errOpenString) {
					pendingKey, pending = key, value
				} else if err != nil {
					return nil, fmt.Errorf("codex: cannot read %s as a string", key)
				} else {
					values[key] = parsed
				}
			}
		} else if pendingKey != "" {
			pending += line
		}
		multiline, depth = tomlLineState(line, multiline, depth)
		if pendingKey != "" && multiline == "" {
			parsed, err := codexSettingString(pending)
			if err != nil {
				return nil, fmt.Errorf("codex: cannot read %s as a string", pendingKey)
			}
			values[pendingKey], pendingKey = parsed, ""
		}
	}
	return values, nil
}

// codexFieldLine splits a "key = value" line whose key is a supported preference.
func codexFieldLine(line string) (key, value string, ok bool) {
	key, value, ok = strings.Cut(strings.TrimSpace(line), "=")
	if !ok {
		return "", "", false
	}
	key = tomlKeyName(key)
	for _, field := range CodexFields() {
		if field.Key == key {
			return key, strings.TrimSpace(value), true
		}
	}
	return "", "", false
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
	if strings.HasPrefix(value, "\"\"\"") || strings.HasPrefix(value, "'''") {
		var err error
		if parsed, rest, err = codexTripleString(value); err != nil {
			return "", err
		}
	} else if strings.HasPrefix(value, "'") {
		end := strings.IndexByte(value[1:], '\'')
		if end < 0 {
			return "", errors.New("unterminated literal string")
		}
		parsed, rest = value[1:end+1], value[end+2:]
	} else {
		quoted, err := strconv.QuotedPrefix(value)
		if err != nil || !strings.HasPrefix(quoted, "\"") {
			return "", errors.New("expected quoted string")
		}
		parsed, err = strconv.Unquote(quoted)
		if err != nil {
			return "", err
		}
		rest = value[len(quoted):]
	}
	if rest = strings.TrimSpace(rest); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", errors.New("unexpected text after string")
	}
	return parsed, nil
}

// CodexSettingsDrift includes stale managed keys whose override was removed from the vault.
func CodexSettingsDrift(home string) (bool, error) {
	path, err := CodexConfigPath(home)
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	overlay, err := LoadCodexOverlay()
	if err != nil {
		return false, err
	}
	next, err := patchCodexSettings(string(raw), overlay)
	return !bytes.Equal(raw, []byte(next)), err
}

// ApplyCodexSettings applies only saved preferences, leaving provider generation and other agents alone.
func ApplyCodexSettings(home string) error {
	path, err := CodexConfigPath(home)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	existed := err == nil
	overlay, err := LoadCodexOverlay()
	if err != nil {
		return err
	}
	next, err := patchCodexSettings(string(old), overlay)
	if err != nil || bytes.Equal(old, []byte(next)) {
		return err
	}
	if _, err := readCodexSettings(next); err != nil {
		return err
	}
	if err := writeCodexConfig(path, old, []byte(next), existed); err != nil {
		return err
	}
	return SaveCodexBaseline(path, []byte(next))
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
	return withCodexLock(path, func() error {
		target, err := codexWriteTarget(path)
		if err != nil {
			return err
		}
		check := func() error {
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
	})
}

func restoreCodexConfig(path string, dryRun bool, out io.Writer) error {
	if dryRun {
		return restoreCodexConfigLocked(path, true, out)
	}
	return withCodexLock(path, func() error { return restoreCodexConfigLocked(path, false, out) })
}

func restoreCodexConfigLocked(path string, dryRun bool, out io.Writer) error {
	backup, found, err := LatestCodexBackup(path)
	if err != nil {
		return err
	}
	source := "encrypted vault backup"
	if !found {
		src, _, ok := legacyBackupSource(path)
		if !ok {
			fmt.Fprintf(out, "codex: no vault backup to restore for %s\n", path)
			return nil
		}
		source = "legacy plaintext backup " + src
		if !dryRun {
			data, err := os.ReadFile(src)
			if err != nil {
				return err
			}
			backup = CodexConfigBackup{Data: data, Existed: true}
		}
	}
	if dryRun {
		fmt.Fprintf(out, "codex: would restore %s from %s\n", path, source)
		return nil
	}
	overlay := map[string]string{}
	start, end, err := codexSettingsSection(string(backup.Data))
	if err != nil {
		return err
	}
	if start >= 0 {
		overlay, err = readCodexSettings(string(backup.Data[start:end]))
		if err != nil {
			return err
		}
	}
	if err := ValidateCodexOverlay(overlay); err != nil {
		return err
	}
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
	if err := SaveCodexOverlay(overlay); err != nil {
		return fmt.Errorf("codex: file restored but preferences could not be synchronized: %w", err)
	}
	if err := SaveCodexBaseline(path, backup.Data); err != nil {
		return err
	}
	if backup.Name != "" { // legacy files are left in place
		if err := ConsumeCodexBackup(backup.Name); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "codex: restored %s from %s\n", path, source)
	return nil
}
