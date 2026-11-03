// overlay_debug.go — COWORKER_OVERLAY_DEBUG: each coworker child's env names and salted value fingerprints, never values.
package agent

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// OverlayDebugVar names the 0600 JSONL file RecordOverlayDebug appends to; unset (or no _SALT) records nothing.
const OverlayDebugVar = "COWORKER_OVERLAY_DEBUG"

type overlayDebugRecord struct {
	Label string            `json:"label"`
	Agent string            `json:"agent"`
	PID   int               `json:"pid"`
	Names []string          `json:"names"`
	FP    map[string]string `json:"fp"`
}

// EnableOverlayDebug turns recording on for this process and its children (ask --overlay-debug <file>). The path is
// made absolute so nested dispatches write the same file; its salt lives beside it as <file>.salt (0600), reused when
// present so every run appended to one file fingerprints with one salt.
func EnableOverlayDebug(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if os.Getenv(OverlayDebugVar+"_SALT") == "" {
		salt, err := overlayDebugSalt(abs + ".salt")
		if err != nil {
			return err
		}
		if err := os.Setenv(OverlayDebugVar+"_SALT", salt); err != nil {
			return err
		}
	}
	return os.Setenv(OverlayDebugVar, abs)
}

// overlayDebugSalt reads the salt beside the log, or creates it exclusively (a concurrent first run reads the winner's).
func overlayDebugSalt(path string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		salt := hex.EncodeToString(b)
		_, werr := f.WriteString(salt)
		return salt, errors.Join(werr, f.Close())
	}
	if !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	got, err := os.ReadFile(path)
	if err != nil || len(got) == 0 {
		return "", errors.Join(err, errors.New("empty salt file "+path))
	}
	return strings.TrimSpace(string(got)), nil
}

// RecordOverlayDebug appends env's names and HMAC-SHA256 fingerprints (last duplicate wins, as exec does).
func RecordOverlayDebug(agentName string, env []string) {
	path, salt := os.Getenv(OverlayDebugVar), os.Getenv(OverlayDebugVar+"_SALT")
	if path == "" || salt == "" {
		return
	}
	rec := overlayDebugRecord{Label: os.Getenv(OverlayDebugVar + "_LABEL"), Agent: agentName, PID: os.Getpid(), FP: map[string]string{}}
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		m := hmac.New(sha256.New, []byte(salt))
		m.Write([]byte(val))
		if _, seen := rec.FP[name]; !seen {
			rec.Names = append(rec.Names, name)
		}
		rec.FP[name] = hex.EncodeToString(m.Sum(nil))[:16]
	}
	slices.Sort(rec.Names)
	line, err := json.Marshal(rec)
	if err == nil {
		err = appendOverlayDebug(path, append(line, '\n'))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-code: overlay debug:", err)
	}
}

func appendOverlayDebug(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil { // a pre-existing file keeps its mode on open
		return errors.Join(err, f.Close())
	}
	_, werr := f.Write(line)
	return errors.Join(werr, f.Close())
}
