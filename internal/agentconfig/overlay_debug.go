// overlay_debug.go — COWORKER_OVERLAY_DEBUG: a 0600 JSONL log of each child's env names with salted fingerprints.
package agentconfig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

// overlayDebugVar names the 0600 JSONL file RecordOverlayDebug appends to; unset (or no _SALT) records nothing.
const overlayDebugVar = "COWORKER_OVERLAY_DEBUG"

const overlayDebugSaltVar = overlayDebugVar + "_SALT"

// OverlayDebugOn reports whether RecordOverlayDebug writes: COWORKER_OVERLAY_DEBUG and its _SALT are both set.
func OverlayDebugOn() bool {
	path, salt := overlayDebugEnv()
	return path != "" && salt != ""
}

func overlayDebugEnv() (path, salt string) {
	return os.Getenv(overlayDebugVar), os.Getenv(overlayDebugSaltVar)
}

type overlayDebugRecord struct {
	Label string            `json:"label"`
	Agent string            `json:"agent"`
	PID   int               `json:"pid"`
	Names []string          `json:"names"`
	FP    map[string]string `json:"fp"`
}

// RecordOverlayDebug appends env's names and HMAC-SHA256 fingerprints (last duplicate wins, as exec does).
func RecordOverlayDebug(agentName string, env []string) {
	path, salt := overlayDebugEnv()
	if path == "" || salt == "" {
		return
	}
	rec := overlayDebugRecord{Label: os.Getenv(overlayDebugVar + "_LABEL"), Agent: agentName, PID: os.Getpid(), FP: map[string]string{}}
	m := hmac.New(sha256.New, []byte(salt)) // one MAC reused across names
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, proc.TreeTagPrefix) {
			continue // a per-run random name: noise in a names diff
		}
		m.Reset()
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
