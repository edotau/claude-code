// Package desktop configures a gateway profile without embedding gateway credentials.
// The vendor schema is unverified, especially inferenceGatewayAuth and its credentialHelper object.
// ttlSeconds and silentRefresh are inferred helper fields; Configure reports this uncertainty on every plan.
// Windows helper commands use Windows argv quoting and invoke WSL directly, without a Linux shell layer.
package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// ProfileID is the stable identity of the harness-owned Desktop profile.
const ProfileID = "bb5e4ae0-46be-4e48-8f41-99846211e9d5"
const stateName = ".claude-code-desktop-state.json"

// Model is a static gateway model exposed to Desktop.
type Model struct {
	Name          string `json:"name"`
	LabelOverride string `json:"labelOverride"`
}

// Options controls the Desktop profile and its reversible activation.
type Options struct {
	Root, BaseURL, Profile, HelperPath string
	HelperArgs                         []string
	HelperWindows                      bool
	Models                             []Model
	Headers                            map[string]string
	Revert, DryRun                     bool
}

type profile struct {
	InferenceProvider               string            `json:"inferenceProvider"`
	InferenceGatewayBaseURL         string            `json:"inferenceGatewayBaseUrl"`
	InferenceGatewayAuthScheme      string            `json:"inferenceGatewayAuthScheme"`
	InferenceCredentialKind         string            `json:"inferenceCredentialKind"`
	InferenceCredentialHelper       string            `json:"inferenceCredentialHelper"`
	InferenceCredentialHelperTO     int               `json:"inferenceCredentialHelperTimeoutSec"`
	InferenceCredentialHelperArgs   []string          `json:"inferenceCredentialHelperArgs"`
	InferenceCredentialHelperTTL    int               `json:"inferenceCredentialHelperTtlSec"`
	InferenceCredentialHelperSilent bool              `json:"inferenceCredentialHelperSilentRefreshEnabled"`
	ModelDiscoveryEnabled           bool              `json:"modelDiscoveryEnabled"`
	InferenceModels                 []Model           `json:"inferenceModels"`
	InferenceCustomHeaders          map[string]string `json:"inferenceCustomHeaders"`
}
type savedState struct {
	Schema            int             `json:"schema"`
	ProfileID         string          `json:"profileId"`
	PreviousPresent   bool            `json:"previousPresent"`
	PreviousAppliedID json.RawMessage `json:"previousAppliedId"`
}

// Configure transactionally updates a profile, its activation metadata, and the owned revert state.
func Configure(opts Options, w io.Writer) error {
	var err error
	if opts.Root == "" {
		opts.Root, err = Root()
		if err != nil {
			return err
		}
	}
	if !opts.Revert && opts.HelperPath == "" {
		opts.HelperPath, opts.HelperArgs, opts.HelperWindows, err = HelperSpec(opts.Profile)
		if err != nil {
			return err
		}
	}
	return configure(opts, w, fileOps{write: paths.AtomicWrite, remove: os.Remove}, targetPath)
}

func configure(opts Options, w io.Writer, ops fileOps, display func(string) (string, error)) error {
	if w == nil {
		w = io.Discard
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return err
	}
	opts.Root = root
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("desktop root %s is unavailable; open Desktop first", root)
	}
	run := func() error { return configureLocked(opts, w, ops, display) }
	if opts.DryRun {
		return run()
	}
	return paths.WithFileLock(filepath.Join(root, ".claude-code-desktop"), run)
}

func configureLocked(opts Options, w io.Writer, ops fileOps, display func(string) (string, error)) error {
	library := filepath.Join(opts.Root, "configLibrary")
	if err := checkLibrary(library); err != nil {
		return err
	}
	profilePath := filepath.Join(library, ProfileID+".json")
	metaPath := filepath.Join(library, "_meta.json")
	statePath := filepath.Join(opts.Root, stateName)
	snapshots := map[string]snapshot{}
	for _, p := range []string{profilePath, metaPath, metaPath + ".bak", metaPath + ".revert.bak", statePath} {
		s, err := capture(p)
		if err != nil {
			return err
		}
		snapshots[p] = s
	}
	meta := map[string]json.RawMessage{}
	if snapshots[metaPath].exists {
		if err := decodeObject(snapshots[metaPath].data, &meta); err != nil {
			return fmt.Errorf("invalid Desktop metadata: %w", err)
		}
	}
	entries, err := profileEntries(meta)
	if err != nil {
		return err
	}
	state, err := loadSavedState(snapshots[statePath])
	if err != nil {
		return err
	}
	hasState := snapshots[statePath].exists
	if !hasState {
		var currentID string
		if json.Unmarshal(meta["appliedId"], &currentID) == nil && currentID == ProfileID {
			return errors.New("desktop profile is already applied without saved revert state")
		}
	}
	filtered := withoutOurProfile(entries)
	var changes []change
	if opts.Revert {
		if !hasState {
			if snapshots[profilePath].exists || len(filtered) != len(entries) {
				return errors.New("cannot revert Desktop profile without saved state")
			}
			_, err := fmt.Fprintln(w, "Desktop profile is not configured; nothing to revert")
			return err
		}
		changes = revertChanges(meta, state, filtered, snapshots[metaPath], profilePath, metaPath, statePath)
	} else {
		if err := validateOptions(&opts); err != nil {
			return err
		}
		changes = applyChanges(opts, meta, hasState, filtered, snapshots, profilePath, metaPath, statePath)
	}
	if err := previewChanges(w, changes, opts.Headers, display); err != nil {
		return err
	}
	if opts.DryRun {
		return nil
	}
	return transact(changes, snapshots, ops, library)
}

// checkLibrary refuses a configLibrary that exists as anything but a real directory.
func checkLibrary(library string) error {
	if fi, err := os.Lstat(library); err == nil {
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return errors.New("configLibrary is not a regular directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// loadSavedState decodes and validates the revert state; a missing file is the zero state.
func loadSavedState(snap snapshot) (savedState, error) {
	var state savedState
	if !snap.exists {
		return state, nil
	}
	raw := snap.data
	var fields map[string]json.RawMessage
	if err := decodeObject(raw, &fields); err != nil {
		return state, fmt.Errorf("invalid Desktop state: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return state, fmt.Errorf("invalid Desktop state: %w", err)
	}
	presence, present := fields["previousPresent"]
	validPresence := bytes.Equal(presence, []byte("true")) || bytes.Equal(presence, []byte("false"))
	if state.Schema != 1 || state.ProfileID != ProfileID || !present || !validPresence || state.PreviousAppliedID == nil ||
		!validAppliedID(state.PreviousAppliedID) ||
		(!state.PreviousPresent && !bytes.Equal(bytes.TrimSpace(state.PreviousAppliedID), []byte("null"))) {
		return state, errors.New("invalid Desktop state fields")
	}
	return state, nil
}

// withoutOurProfile drops this harness's row; capacity leaves room to re-add it.
func withoutOurProfile(entries []json.RawMessage) []json.RawMessage {
	filtered := make([]json.RawMessage, 0, len(entries)+1)
	for _, entry := range entries {
		var row map[string]json.RawMessage
		_ = json.Unmarshal(entry, &row)
		var id string
		_ = json.Unmarshal(row["id"], &id)
		if id != ProfileID {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// revertChanges restores the previous applied id in meta and lists the revert's file changes.
func revertChanges(meta map[string]json.RawMessage, state savedState, filtered []json.RawMessage, metaSnap snapshot, profilePath, metaPath, statePath string) []change {
	changes := []change{}
	if appliedIsOurs(meta["appliedId"]) {
		if state.PreviousPresent {
			meta["appliedId"] = state.PreviousAppliedID
		} else {
			delete(meta, "appliedId")
		}
	}
	setEntries(meta, filtered)
	if metaSnap.exists {
		changes = append(changes, change{path: metaPath + ".revert.bak", data: metaSnap.data})
	}
	return append(changes, change{path: metaPath, data: marshal(meta)}, change{path: profilePath, remove: true}, change{path: statePath, remove: true})
}

// applyChanges points meta at our profile and lists the apply's file changes.
func applyChanges(opts Options, meta map[string]json.RawMessage, hasState bool, filtered []json.RawMessage,
	snapshots map[string]snapshot, profilePath, metaPath, statePath string) []change {
	changes := []change{}
	if !hasState {
		previous, present := meta["appliedId"]
		if !present {
			previous = json.RawMessage("null")
		}
		state := savedState{Schema: 1, ProfileID: ProfileID, PreviousPresent: present, PreviousAppliedID: previous}
		changes = append(changes, change{path: statePath, data: marshal(state)})
	}
	if m := snapshots[metaPath]; m.exists && !snapshots[metaPath+".bak"].exists {
		changes = append(changes, change{path: metaPath + ".bak", data: m.data})
	}
	filtered = append(filtered, marshal(map[string]string{"id": ProfileID, "name": opts.Profile}))
	setEntries(meta, filtered)
	meta["appliedId"] = marshal(ProfileID)
	p := profile{InferenceProvider: "gateway", InferenceGatewayBaseURL: opts.BaseURL,
		InferenceGatewayAuthScheme: "bearer", InferenceCredentialKind: "helper-script",
		InferenceCredentialHelper: opts.HelperPath, InferenceCredentialHelperArgs: opts.HelperArgs,
		InferenceCredentialHelperTTL: 900, InferenceCredentialHelperTO: 300, InferenceCredentialHelperSilent: true,
		InferenceModels: opts.Models, InferenceCustomHeaders: opts.Headers}
	return append(changes, change{path: profilePath, data: marshal(p)}, change{path: metaPath, data: marshal(meta)})
}

// previewChanges prints the notes, then every planned change with its contents redacted.
func previewChanges(w io.Writer, changes []change, headers map[string]string, display func(string) (string, error)) error {
	if _, err := fmt.Fprintln(w, "NOTE: configLibrary layout and _meta.json are inferred, not documented;\n"+
		"  managed (MDM/registry) config overrides them. Quit and reopen Desktop to apply."); err != nil {
		return err
	}
	if desktopRunning() {
		if _, err := fmt.Fprintln(w, "WARNING: Claude Desktop is running; restart Claude Desktop to apply (not closed)."); err != nil {
			return err
		}
	}
	for _, c := range changes {
		target, err := display(c.path)
		if err != nil {
			return err
		}
		if c.remove {
			if _, err := fmt.Fprintf(w, "remove %s\n", target); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(w, "write %s\n%s", target, redact(c.data, headers)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOptions(opts *Options) error {
	if err := validateGatewayURL(opts.BaseURL); err != nil {
		return err
	}
	if !helperAbsolute(opts.HelperPath, opts.HelperWindows) {
		return errors.New("desktop credential helper must be an absolute path")
	}
	if opts.Profile == "" {
		opts.Profile = "claude-code gateway"
	}
	if len(opts.Models) == 0 {
		return errors.New("desktop requires at least one static model")
	}
	if err := validateModels(opts.Models); err != nil {
		return err
	}
	if opts.Headers == nil {
		opts.Headers = map[string]string{}
	}
	for k := range opts.Headers {
		if err := validateHeaderName(k); err != nil {
			return err
		}
	}
	return nil
}

func validateGatewayURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("desktop gateway URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	return nil
}

func validateModels(models []Model) error {
	seen := map[string]bool{}
	for _, m := range models {
		if strings.TrimSpace(m.Name) == "" || seen[m.Name] {
			return errors.New("desktop model names must be nonempty and unique")
		}
		seen[m.Name] = true
	}
	return nil
}

func validateHeaderName(k string) error {
	if strings.TrimSpace(k) == "" || strings.ContainsAny(k, "\r\n") {
		return errors.New("invalid Desktop custom header name")
	}
	if strings.EqualFold(k, "authorization") || strings.EqualFold(k, "proxy-authorization") || strings.EqualFold(k, "x-api-key") {
		return errors.New("authentication headers must come from the Desktop credential helper")
	}
	return nil
}

// legacyEntriesKey is the list key this package wrote before matching Desktop's (and ucode's) "entries".
const legacyEntriesKey = "profiles"

// setEntries writes the profile list under "entries", folding away a legacy "profiles" list.
func setEntries(meta map[string]json.RawMessage, entries []json.RawMessage) {
	delete(meta, legacyEntriesKey)
	meta["entries"] = marshal(entries)
}

func profileEntries(meta map[string]json.RawMessage) ([]json.RawMessage, error) {
	if id, ok := meta["appliedId"]; ok && !validAppliedID(id) {
		return nil, errors.New("invalid Desktop appliedId")
	}
	var entries []json.RawMessage
	for _, key := range []string{"entries", legacyEntriesKey} {
		raw, ok := meta[key]
		if !ok {
			continue
		}
		var list []json.RawMessage
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &list) != nil {
			return nil, fmt.Errorf("invalid Desktop %s array", key)
		}
		entries = append(entries, list...)
	}
	for _, entry := range entries {
		var row map[string]json.RawMessage
		if err := decodeObject(entry, &row); err != nil {
			return nil, errors.New("invalid Desktop profile entry")
		}
		var id string
		if json.Unmarshal(row["id"], &id) != nil || id == "" {
			return nil, errors.New("invalid Desktop profile id")
		}
	}
	return entries, nil
}

func validAppliedID(raw []byte) bool {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	var s string
	return json.Unmarshal(raw, &s) == nil
}

func marshal(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

func redact(raw []byte, headers map[string]string) []byte {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return []byte("[redacted]\n")
	}
	return marshal(redactNode(v, headers))
}

// redactNode blanks custom-header values and any string equal to one, in place.
func redactNode(node any, headers map[string]string) any {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if strings.EqualFold(k, "inferenceCustomHeaders") {
				redactHeaders(v)
			} else {
				n[k] = redactNode(v, headers)
			}
		}
	case []any:
		for i, v := range n {
			n[i] = redactNode(v, headers)
		}
	case string:
		for _, value := range headers {
			if value != "" && n == value {
				return "[REDACTED]"
			}
		}
	}
	return node
}

func redactHeaders(v any) {
	if h, ok := v.(map[string]any); ok {
		for key := range h {
			h[key] = "[REDACTED]"
		}
	}
}

func decodeObject(raw []byte, dst *map[string]json.RawMessage) error {
	if !json.Valid(raw) {
		return errors.New("invalid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := uniqueJSON(dec); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return err
	}
	if *dst == nil {
		return errors.New("expected JSON object")
	}
	return nil
}

func uniqueJSON(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for dec.More() {
		if delim == '{' {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			s := key.(string)
			if seen[s] {
				return fmt.Errorf("duplicate JSON key %q", s)
			}
			seen[s] = true
		}
		if err := uniqueJSON(dec); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

func appliedIsOurs(raw json.RawMessage) bool {
	var id string
	return json.Unmarshal(raw, &id) == nil && id == ProfileID
}

func schemaPath(parts ...string) string {
	root, err := Root()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

var desktopStateFormat = schemas.Register(schemas.Format{
	Name: "desktop-state", Owner: "internal/desktop", Kind: schemas.Record, Version: 1,
	Glob:    func() string { return schemaPath(stateName) },
	New:     func() any { return new(savedState) },
	Keep:    "previous Desktop activation until explicit revert",
	Migrate: schemas.SameShape,
})

var _ = schemas.Register(schemas.Format{
	Name: "desktop-profile", Owner: "internal/desktop", Kind: schemas.Derived, Version: 0,
	Glob: func() string { return schemaPath("configLibrary", ProfileID+".json") },
	Keep: "foreign Desktop profile; inferred vendor schema, regenerated by desktop configure",
})

var _ = schemas.Register(schemas.Format{
	Name: "desktop-meta", Owner: "internal/desktop", Kind: schemas.Derived, Version: 0,
	Glob: func() string { return schemaPath("configLibrary", "_meta.json") },
	Keep: "foreign Desktop activation metadata; preserve unknown fields and other profiles",
})
