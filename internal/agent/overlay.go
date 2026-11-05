// overlay.go — folds a resolved COWORKER_OVERLAY (agentconfig) into a headless RunOptions or an interactive Plan.
package agent

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
)

// overlayIdle reports a launch the overlay changes nothing about: empty layer and no $COWORKER_OVERLAY to hide.
func overlayIdle(r agentconfig.Resolved) bool {
	return len(r.Set)+len(r.Unset)+len(r.Keep) == 0 && os.Getenv(agentconfig.OverlayVar) == ""
}

// ApplyOverlay layers r under req's --env (the shorthand wins) and unsets $COWORKER_OVERLAY in the child (one hop).
func ApplyOverlay(req RunOptions, r agentconfig.Resolved) RunOptions {
	if overlayIdle(r) {
		return req
	}
	env := make([]string, 0, len(r.Set)+len(req.Env))
	for _, k := range slices.Sorted(maps.Keys(r.Set)) {
		env = append(env, k+"="+r.Set[k])
	}
	req.Env = append(env, req.Env...)
	req.KeepEnv = slices.Concat(req.KeepEnv, r.Keep)
	req.Unset = slices.Concat(req.Unset, r.Unset, []string{agentconfig.OverlayVar})
	req.OverlaySet = slices.Sorted(maps.Keys(r.Set))
	return req
}

// CheckRouteNames refuses a name in names that route (KEY=VALUE, before the overlay is appended) already sets.
func CheckRouteNames(route, names []string) error {
	own := map[string]bool{}
	for _, kv := range route {
		k, _, _ := strings.Cut(kv, "=")
		own[k] = true
	}
	for _, n := range names {
		if own[n] {
			return fmt.Errorf("overlay set %s: set by the launch for its route; use --profile", n)
		}
	}
	return nil
}

// CheckOverlay refuses an overlay that sets or unsets a name the plan's launch sets for its route.
func (p Plan) CheckOverlay(r agentconfig.Resolved) error {
	for _, k := range slices.Sorted(maps.Keys(r.Set)) {
		if _, own := p.Set[k]; own {
			return fmt.Errorf("overlay set %s: set by the launch for its route; use --profile", k)
		}
	}
	for _, k := range r.Unset {
		if _, own := p.Set[k]; own {
			return fmt.Errorf("overlay unset %s: set by the launch for its route; use --profile", k)
		}
	}
	return nil
}

// WithOverlay layers r over an interactive plan the way ApplyOverlay does for a headless run; the launch's own Set wins.
func (p Plan) WithOverlay(r agentconfig.Resolved) Plan {
	if overlayIdle(r) {
		return p
	}
	set := maps.Clone(r.Set)
	if set == nil {
		set = map[string]string{}
	}
	maps.Copy(set, p.Set)
	p.Set, p.Keep = set, slices.Concat(p.Keep, r.Keep)
	unset := slices.DeleteFunc(slices.Clone(r.Unset), func(k string) bool { _, own := p.Set[k]; return own })
	p.Unset = slices.Concat(p.Unset, unset, []string{agentconfig.OverlayVar})
	return p
}

// EnableOverlay checks path and points this process and its --job relaunches at it (absolute).
func EnableOverlay(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := agentconfig.LoadOverlay(abs); err != nil {
		return err
	}
	return os.Setenv(agentconfig.OverlayVar, abs)
}

// COWORKER_OVERLAY_DEBUG: each coworker child's env names and salted value fingerprints, never values.

// OverlayDebugVar names the 0600 JSONL file RecordOverlayDebug appends to; unset (or no _SALT) records nothing.
const OverlayDebugVar = "COWORKER_OVERLAY_DEBUG"

// OverlayDebugOn reports whether RecordOverlayDebug writes: COWORKER_OVERLAY_DEBUG and its _SALT are both set.
func OverlayDebugOn() bool {
	return os.Getenv(OverlayDebugVar) != "" && os.Getenv(OverlayDebugVar+"_SALT") != ""
}

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

// overlayDebugSalt reads the salt beside the log, or links a fully written one into place (the loser reads the winner's).
func overlayDebugSalt(path string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	salt := hex.EncodeToString(b)
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // the temp name only; path keeps the hard link
	_, werr := tmp.WriteString(salt)
	if err := errors.Join(werr, tmp.Chmod(0o600), tmp.Close()); err != nil {
		return "", err
	}
	// link is atomic and fails if path exists, so a reader never sees a half-written salt
	if err := os.Link(tmp.Name(), path); err == nil {
		return salt, nil
	} else if !errors.Is(err, os.ErrExist) {
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
