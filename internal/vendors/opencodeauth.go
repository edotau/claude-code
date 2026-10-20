package vendors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// OpencodeConn is one stored or environment-backed opencode connection; it never carries a credential value.
type OpencodeConn struct {
	Kind      string // "credential" | "env"
	ID        string // 2.x credential ID; 1.x = integration ID
	Label     string // 2.x label; 1.x "OAuth" | "API key"; env = variable name
	Method    string // "oauth" | "key" | "external" | ""
	Active    bool   // set only when 2.x JSON carries it; never guessed
	NeedsAuth string // connection status message when status == "needs_auth"
}

// OpencodeIntegration is one opencode integration (provider) and its connections.
type OpencodeIntegration struct {
	ID, Name string
	Conns    []OpencodeConn
}

// opencodeOutput is the exec seam for the installed opencode CLI (stdout only); tests replace it.
var opencodeOutput = func(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := paths.VendorBin("opencode")
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, bin, args...).Output()
}

const opencodeAuthTimeout = 5 * time.Second

var (
	opencodeMu        sync.Mutex
	opencodeMajorKey  string
	opencodeMajorVal  int
	opencodeMajorSet  bool
	opencodeAuthKey   string
	opencodeAuthVal   []OpencodeIntegration
	opencodeAuthErr   error
	opencodeAuthIsSet bool
)

var opencodeVersionRe = regexp.MustCompile(`(\d+)\.\d+`)

// opencodeBinKey identifies the installed opencode binary (path+size+mtime), so a change or upgrade re-probes.
func opencodeBinKey() string {
	p, err := paths.VendorBin("opencode")
	if err != nil {
		return ""
	}
	if fi, err := os.Stat(p); err == nil {
		return fmt.Sprintf("%s|%d|%d", p, fi.Size(), fi.ModTime().UnixNano())
	}
	return p
}

// OpencodeMajor is the major version from `opencode --version` (cached per binary); 0 = absent or unparseable.
func OpencodeMajor() int {
	key := opencodeBinKey()
	opencodeMu.Lock()
	defer opencodeMu.Unlock()
	return opencodeMajorLocked(key)
}

func opencodeMajorLocked(key string) int {
	if opencodeMajorSet && opencodeMajorKey == key {
		return opencodeMajorVal
	}
	if key != "" {
		if c := loadOpencodeDiskCache(); c.Version != nil && c.Version.Bin == key {
			opencodeMajorKey, opencodeMajorVal, opencodeMajorSet = key, c.Version.Major, true
			return c.Version.Major
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), opencodeAuthTimeout)
	defer cancel()
	out, err := opencodeOutput(ctx, "--version")
	v := 0
	if err == nil {
		v = parseOpencodeMajor(string(out))
	}
	opencodeMajorKey, opencodeMajorVal, opencodeMajorSet = key, v, true
	if err == nil && v > 0 && strings.Contains(key, "|") {
		updateOpencodeDiskCache(func(c *opencodeDiskCache) { c.Version = &opencodeDiskVersion{Bin: key, Major: v} })
	}
	return v
}

// parseOpencodeMajor reads the major from "1.19.3", "v2.0.32" or "opencode 2.0.32"; 0 when none.
func parseOpencodeMajor(s string) int {
	m := opencodeVersionRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// OpencodeAuth lists opencode's stored logins, sorted by integration ID: 2.x via `opencode auth list --format
// json --standalone`, 1.x (or unknown version) via auth.json. A failed 2.x call still returns the auth.json data,
// plus an error describing the failure. Cached per process (and per binary/auth.json state); see InvalidateOpencodeAuth.
func OpencodeAuth() ([]OpencodeIntegration, error) {
	bin := opencodeBinKey()
	file := opencodeAuthFile()
	key := bin + "\x00" + file + "\x00" + fileStamp(file)
	opencodeMu.Lock()
	defer opencodeMu.Unlock()
	if opencodeAuthIsSet && opencodeAuthKey == key {
		return opencodeAuthVal, opencodeAuthErr
	}
	var ints []OpencodeIntegration
	var err error
	if opencodeMajorLocked(bin) >= 2 {
		ints, err = opencodeAuthV2Cached(bin)
		if err != nil {
			fallback, ferr := readOpencodeAuthJSON(file)
			if ferr != nil && !errors.Is(ferr, os.ErrNotExist) {
				err = errors.Join(err, ferr)
			}
			ints = fallback
		}
	} else {
		ints, err = readOpencodeAuthJSON(file)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	opencodeAuthKey, opencodeAuthVal, opencodeAuthErr, opencodeAuthIsSet = key, ints, err, true
	return ints, err
}

// InvalidateOpencodeAuth drops the cached logins; call after a login, logout or switch.
func InvalidateOpencodeAuth() {
	opencodeMu.Lock()
	opencodeAuthIsSet, opencodeAuthVal, opencodeAuthErr = false, nil, nil
	if _, err := os.Stat(opencodeCacheFile()); err == nil {
		updateOpencodeDiskCache(func(c *opencodeDiskCache) { c.Auth = nil })
	}
	opencodeMu.Unlock()
}

// fileStamp is path's size+mtime, or "-" when absent, so an auth.json rewrite misses the cache.
func fileStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%d|%d", fi.Size(), fi.ModTime().UnixNano())
}

// opencodeListEntry is one integration in `opencode auth list --format json` (schema/src/connection.ts).
type opencodeListEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Connections []struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		Label  string `json:"label"`
		Name   string `json:"name"`
		Method string `json:"method"`
		Active *bool  `json:"active"`
		Status *struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"status"`
	} `json:"connections"`
}

func opencodeAuthV2() ([]OpencodeIntegration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), opencodeAuthTimeout)
	defer cancel()
	out, err := opencodeOutput(ctx, "auth", "list", "--format", "json", "--standalone")
	if err != nil {
		return nil, fmt.Errorf("opencode auth list: %s", execErrDetail(err))
	}
	ints, err := parseOpencodeAuthList(out)
	if err != nil {
		return nil, fmt.Errorf("opencode auth list: %w", err)
	}
	return ints, nil
}

// execErrDetail is err plus the first stderr line, when the command reported one.
func execErrDetail(err error) string {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if line, _, _ := strings.Cut(strings.TrimSpace(string(ee.Stderr)), "\n"); line != "" {
			if len(line) > 200 {
				line = line[:200]
			}
			return err.Error() + ": " + line
		}
	}
	return err.Error()
}

func parseOpencodeAuthList(data []byte) ([]OpencodeIntegration, error) {
	var entries []opencodeListEntry
	if err := json.Unmarshal(bytes.TrimSpace(data), &entries); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	ints := make([]OpencodeIntegration, 0, len(entries))
	for _, e := range entries {
		in := OpencodeIntegration{ID: strings.TrimSuffix(e.ID, "/"), Name: e.Name}
		if in.Name == "" {
			in.Name = in.ID
		}
		for _, c := range e.Connections {
			conn := OpencodeConn{Kind: c.Type}
			switch c.Type {
			case "env":
				conn.Label = c.Name
			default:
				conn.ID, conn.Label, conn.Method = c.ID, c.Label, c.Method
				if c.Active != nil {
					conn.Active = *c.Active
				}
			}
			if c.Status != nil && c.Status.Status == "needs_auth" {
				conn.NeedsAuth = c.Status.Message
			}
			in.Conns = append(in.Conns, conn)
		}
		ints = append(ints, in)
	}
	sort.SliceStable(ints, func(i, j int) bool { return ints[i].ID < ints[j].ID })
	return ints, nil
}

// readOpencodeAuthJSON maps 1.x auth.json (keys = integration IDs) to integrations; only "type" is decoded.
func readOpencodeAuthJSON(file string) ([]OpencodeIntegration, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var m map[string]struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("opencode auth.json: %w", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ints []OpencodeIntegration
	for _, k := range keys {
		id := strings.TrimSuffix(k, "/")
		conn := OpencodeConn{Kind: "credential", ID: id}
		if m[k].Type == "oauth" {
			conn.Method, conn.Label = "oauth", "OAuth"
		} else {
			conn.Method, conn.Label = "key", "API key"
		}
		// "x" and "x/" are the same integration: merge rather than list it twice.
		if n := len(ints); n > 0 && ints[n-1].ID == id {
			ints[n-1].Conns = append(ints[n-1].Conns, conn)
			continue
		}
		ints = append(ints, OpencodeIntegration{ID: id, Name: id, Conns: []OpencodeConn{conn}})
	}
	return ints, nil
}

// OpencodeLogins lists the integration IDs opencode has at least one connection for (any version), sorted.
func OpencodeLogins() []string {
	ints, _ := OpencodeAuth()
	var names []string
	for _, in := range ints {
		if len(in.Conns) > 0 {
			names = append(names, in.ID)
		}
	}
	return names
}

// OpencodeLogoutArgv is the non-interactive logout command: 2.x needs the credential, 1.x only the
// integration. An unresolved bin/opencode (missing or a broken link) is an error, never a bare "opencode"
// PATH guess, so callers report the same "not installed / broken link" message as everywhere else.
func OpencodeLogoutArgv(id, credID string) ([]string, error) {
	bin, err := opencodeBin()
	if err != nil {
		return nil, err
	}
	if OpencodeMajor() >= 2 {
		return []string{bin, "auth", "logout", id, credID}, nil
	}
	return []string{bin, "auth", "logout", id}, nil
}

// OpencodeSwitchArgv makes credID the active account for id (2.x only); nil, nil on 1.x (no switch verb).
func OpencodeSwitchArgv(id, credID string) ([]string, error) {
	if OpencodeMajor() < 2 {
		return nil, nil
	}
	bin, err := opencodeBin()
	if err != nil {
		return nil, err
	}
	return []string{bin, "auth", "switch", id, credID}, nil
}

// opencodeBin is the installed opencode path; it never falls back to a bare PATH lookup.
func opencodeBin() (string, error) {
	return paths.VendorBin("opencode")
}

// opencodeAuthFile is $XDG_DATA_HOME/opencode/auth.json (default ~/.local/share), opencode 1.x's login store.
func opencodeAuthFile() string { return filepath.Join(opencodeDataDir(), "auth.json") }

// opencodeDataDir is $XDG_DATA_HOME/opencode (default ~/.local/share/opencode).
func opencodeDataDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "opencode")
}

// opencodeDBPath is the 2.x login database; "" for an in-memory DB (never cached).
// Non-stable channels use opencode-<channel>.db, which we can't detect cheaply; OPENCODE_DB overrides.
func opencodeDBPath() string {
	db := os.Getenv("OPENCODE_DB")
	switch {
	case db == ":memory:":
		return ""
	case filepath.IsAbs(db):
		return db
	case db == "":
		db = "opencode.db"
	}
	return filepath.Join(opencodeDataDir(), db)
}

// opencodeCacheFile persists probe results across processes (no credential values ever land here).
func opencodeCacheFile() string {
	return filepath.Join(paths.StateDir(), "vendors", "opencode-auth.json")
}

type opencodeDiskVersion struct {
	Bin   string `json:"bin"`
	Major int    `json:"major"`
}

type opencodeDiskAuth struct {
	Bin          string                `json:"bin"`
	DB           string                `json:"db"`
	Integrations []OpencodeIntegration `json:"integrations"`
}

type opencodeDiskCache struct {
	Version *opencodeDiskVersion `json:"version,omitempty"`
	Auth    *opencodeDiskAuth    `json:"auth,omitempty"`
}

// loadOpencodeDiskCache reads the cache; any error yields an empty cache so callers fall through to a live probe.
func loadOpencodeDiskCache() opencodeDiskCache {
	var c opencodeDiskCache
	data, err := os.ReadFile(opencodeCacheFile())
	if err != nil || json.Unmarshal(data, &c) != nil {
		return opencodeDiskCache{}
	}
	return c
}

// updateOpencodeDiskCache read-modify-writes the cache atomically (0600 in a 0700 dir); failures are ignored.
func updateOpencodeDiskCache(fn func(*opencodeDiskCache)) {
	c := loadOpencodeDiskCache()
	fn(&c)
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	file := opencodeCacheFile()
	if os.MkdirAll(filepath.Dir(file), 0o700) != nil {
		return
	}
	_ = paths.AtomicWrite(file, data, 0o600)
}

// opencodeDBKey is the DB path plus size+mtime of it and its -wal; "" when the DB can't be cached.
func opencodeDBKey() string {
	db := opencodeDBPath()
	if db == "" {
		return ""
	}
	return db + "|" + fileStamp(db) + "|" + fileStamp(db+"-wal")
}

// opencodeAuthV2Cached serves `auth list` from disk when binary and DB are unchanged; failures are never cached.
func opencodeAuthV2Cached(bin string) ([]OpencodeIntegration, error) {
	db := opencodeDBKey()
	cacheable := db != "" && strings.Contains(bin, "|")
	if cacheable {
		if a := loadOpencodeDiskCache().Auth; a != nil && a.Bin == bin && a.DB == db {
			return a.Integrations, nil
		}
	}
	ints, err := opencodeAuthV2()
	if err == nil && cacheable {
		updateOpencodeDiskCache(func(c *opencodeDiskCache) { c.Auth = &opencodeDiskAuth{Bin: bin, DB: db, Integrations: ints} })
	}
	return ints, err
}
