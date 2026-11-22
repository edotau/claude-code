package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// OpencodeConn is one stored or environment-backed opencode connection; it never carries a credential value.
type OpencodeConn struct {
	// "credential" | "env"
	Kind string
	// 2.x credential ID; 1.x = integration ID
	ID string
	// 2.x label; 1.x "OAuth" | "API key"; env = variable name
	Label string
	// "oauth" | "key" | "external" | ""
	Method string
	// set only when 2.x JSON carries it; never guessed
	Active bool
	// connection status message when status == "needs_auth"
	NeedsAuth string
}

// OpencodeIntegration is one opencode integration (provider) and its connections.
type OpencodeIntegration struct {
	ID, Name string
	Conns    []OpencodeConn
}

// opencodeOutput is the exec seam for the installed opencode CLI (stdout only); tests replace it.
var opencodeOutput = func(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := paths.AgentBin("opencode")
	if err != nil {
		return nil, err
	}
	// `auth list` reports OPENCODE_API_KEY from env; `--version` needs no login
	login := "opencode"
	if len(args) > 0 && args[0] == "--version" {
		login = ""
	}
	return agent.ProbeCLI(agent.ProbeContext(ctx, login), bin, agent.EnvSpec{Mode: agent.EnvAgent}, false, args...)
}

var (
	opencodeMajorMemo = paths.NewMemo[string, int]("opencode-major")
	opencodeAuthMemo  = paths.NewMemo[string, []OpencodeIntegration]("opencode-auth")
)

// opencodeAuthRetry is how long a failed `auth list` is reused, so callers in one menu pass spawn it once.
const opencodeAuthRetry = 30 * time.Second

// opencodeBinKey identifies the installed opencode binary (path+size+mtime), so a change or upgrade re-probes.
func opencodeBinKey() string {
	p, err := paths.AgentBin("opencode")
	if err != nil {
		return ""
	}
	if key, ok := paths.BinKey(p); ok {
		return key
	}
	return p
}

// OpencodeMajor caches the binary's major version; 0 means absent, unparseable or unprobed.
func OpencodeMajor() int {
	v, _ := opencodeMajor()
	return v
}

// opencodeMajor is OpencodeMajor plus the probe error (never cached); a major-0 answer stays in memory, not on disk.
func opencodeMajor() (int, error) {
	keep := func(_ int, err error) time.Duration {
		if err != nil {
			return -1
		}
		return 0
	}
	return opencodeMajorMemo.Do(opencodeBinKey(), keep, probeOpencodeMajor)
}

func probeOpencodeMajor() (int, error) {
	// "" when absent: the probe reports why
	path, _ := paths.AgentBin("opencode")
	var perr error
	line := tools.ProbedVersionLine(path, func(string) string {
		ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
		defer cancel()
		out, err := opencodeOutput(ctx, "--version")
		if err != nil {
			perr = fmt.Errorf("opencode --version: %s", agent.ProbeErrDetail(err))
			return ""
		}
		first, _, _ := strings.Cut(string(out), "\n")
		if first = strings.TrimSpace(first); textutil.VersionMajor(first) > 0 {
			return first
		}
		return ""
	})
	if perr != nil {
		return 0, perr
	}
	return textutil.VersionMajor(line), nil
}

// OpencodeAuth caches sorted logins per binary/store; failed 2.x probes fall back to auth.json with an error.
func OpencodeAuth() ([]OpencodeIntegration, error) {
	bin, file, key := opencodeAuthKeys()
	unkeyed := false
	keep := func(_ []OpencodeIntegration, err error) time.Duration {
		switch {
		case unkeyed:
			return -1
		case err != nil:
			return opencodeAuthRetry
		}
		return 0
	}
	ints, err := opencodeAuthMemo.Do(key, keep, func() ([]OpencodeIntegration, error) {
		major, merr := opencodeMajor()
		if major >= 2 {
			return opencodeAuthWithFallback(bin, file)
		}
		ints, err := opencodeAuthV1(file)
		// version unknown, so these 1.x files may not be the login store
		if merr != nil && bin != "" {
			unkeyed = true
			return ints, errors.Join(merr, err)
		}
		return ints, err
	})
	return cloneOpencodeIntegrations(ints), err
}

func cloneOpencodeIntegrations(ints []OpencodeIntegration) []OpencodeIntegration {
	out := slices.Clone(ints)
	for i := range out {
		out[i].Conns = slices.Clone(out[i].Conns)
	}
	return out
}

func opencodeAuthV1(file string) ([]OpencodeIntegration, error) {
	ints, err := readOpencodeAuthJSON(file)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return ints, err
}

func opencodeAuthWithFallback(bin, file string) ([]OpencodeIntegration, error) {
	ints, err := opencodeAuthV2Cached(bin)
	if err == nil {
		return ints, nil
	}
	fallback, ferr := readOpencodeAuthJSON(file)
	if ferr != nil && !errors.Is(ferr, os.ErrNotExist) {
		err = errors.Join(err, ferr)
	}
	return fallback, err
}

// opencodeAuthKeys identifies the binary, auth.json and DB/WAL state for the in-memory cache.
func opencodeAuthKeys() (bin, file, key string) {
	bin, file = opencodeBinKey(), opencodeAuthFile()
	return bin, file, bin + "\x00" + file + "\x00" + fileStamp(file) + "\x00" + opencodeDBKey()
}

// InvalidateOpencodeAuth drops the cached logins; call after a login, logout or switch.
func InvalidateOpencodeAuth() {
	opencodeAuthMemo.Drop()
	ownLogin.Drop()
	if _, err := os.Stat(opencodeCacheFile()); err != nil {
		return
	}
	// the disk lock can wait minutes: never under opencodeMu; a failed update drops the rebuildable cache file
	if updateOpencodeDiskCache(func(c *opencodeDiskCache) { c.Auth = nil }) != nil {
		_ = os.Remove(opencodeCacheFile())
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
	defer cancel()
	out, err := opencodeOutput(ctx, "auth", "list", "--format", "json", "--standalone")
	if err != nil {
		return nil, fmt.Errorf("opencode auth list: %s", agent.ProbeErrDetail(err))
	}
	ints, err := parseOpencodeAuthList(out)
	if err != nil {
		return nil, fmt.Errorf("opencode auth list: %w", err)
	}
	return ints, nil
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
	slices.SortStableFunc(ints, func(a, b OpencodeIntegration) int { return strings.Compare(a.ID, b.ID) })
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
	keys := slices.Sorted(maps.Keys(m))
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
		if i := slices.IndexFunc(ints, func(in OpencodeIntegration) bool { return in.ID == id }); i >= 0 {
			ints[i].Conns = append(ints[i].Conns, conn)
			continue
		}
		ints = append(ints, OpencodeIntegration{ID: id, Name: id, Conns: []OpencodeConn{conn}})
	}
	return ints, nil
}

// listOpencodeLogins lists the integration IDs opencode has at least one connection for (any version), sorted.
func listOpencodeLogins() []string {
	names, _ := opencodeLoginsErr()
	return names
}

// opencodeLoginsErr is listOpencodeLogins plus the probe error, so a failed `auth list` is not read as logged out.
func opencodeLoginsErr() ([]string, error) {
	ints, err := OpencodeAuth()
	var names []string
	for _, in := range ints {
		if len(in.Conns) > 0 {
			names = append(names, in.ID)
		}
	}
	return names, err
}

// OpencodeLogoutArgv resolves the binary (never PATH fallback): 2.x logs out a credential, 1.x an integration.
func OpencodeLogoutArgv(id, credID string) ([]string, error) {
	bin, err := paths.AgentBin("opencode")
	if err != nil {
		return nil, err
	}
	major, err := opencodeMajor()
	if err != nil {
		return nil, err
	}
	if major >= 2 {
		return []string{bin, "auth", "logout", id, credID}, nil
	}
	return []string{bin, "auth", "logout", id}, nil
}

// OpencodeSwitchArgv makes credID the active account for id (2.x only); nil, nil on 1.x (no switch verb).
func OpencodeSwitchArgv(id, credID string) ([]string, error) {
	bin, err := paths.AgentBin("opencode")
	if err != nil {
		return nil, err
	}
	major, err := opencodeMajor()
	if err != nil || major < 2 {
		return nil, err
	}
	return []string{bin, "auth", "switch", id, credID}, nil
}

// opencodeAuthFile is $XDG_DATA_HOME/opencode/auth.json (default ~/.local/share), opencode 1.x's login store.
func opencodeAuthFile() string { return filepath.Join(paths.OpencodeDataDir(), "auth.json") }

// opencodeCacheFile persists probe results across processes (no credential values ever land here).
func opencodeCacheFile() string {
	return filepath.Join(paths.StateDir(), "vendors", "opencode-auth.json")
}

type opencodeDiskAuth struct {
	Bin          string                `json:"bin"`
	DB           string                `json:"db"`
	Integrations []OpencodeIntegration `json:"integrations"`
}

type opencodeDiskCache struct {
	Schema int               `json:"schema"`
	Auth   *opencodeDiskAuth `json:"auth,omitempty"`
}

// loadOpencodeDiskCache reads the cache; any error yields an empty cache so callers fall through to a live probe.
func loadOpencodeDiskCache() opencodeDiskCache {
	var c opencodeDiskCache
	data, err := os.ReadFile(opencodeCacheFile())
	if err != nil || !schemas.DecodeOK(opencodeCacheFormat, data, &c) {
		return opencodeDiskCache{}
	}
	return c
}

// updateOpencodeDiskCache read-modify-writes the cache under a file lock (0600 in a 0700 dir) and returns any failure.
func updateOpencodeDiskCache(fn func(*opencodeDiskCache)) error {
	file := opencodeCacheFile()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return paths.WithFileLock(file, func() error {
		c := loadOpencodeDiskCache()
		fn(&c)
		c.Schema = opencodeCacheSchema
		data, err := json.Marshal(c)
		if err != nil {
			return err
		}
		return paths.AtomicWrite(file, data, 0o600)
	})
}

// opencodeDBKey is the DB path plus size+mtime of it and its -wal; "" when the DB can't be cached.
func opencodeDBKey() string {
	db := paths.OpencodeDBPath()
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
		_ = updateOpencodeDiskCache(func(c *opencodeDiskCache) { c.Auth = &opencodeDiskAuth{Bin: bin, DB: db, Integrations: ints} })
	}
	return ints, err
}

// opencodeCacheSchema is the cache's current schema version; a mismatch is a cache miss, never migrated.
const opencodeCacheSchema = 1

var opencodeCacheFormat = schemas.Register(schemas.Format{
	Name: "opencode-auth-cache", Owner: "internal/opencode", Kind: schemas.Cache,
	Glob:    func() string { return opencodeCacheFile() },
	New:     func() any { return &opencodeDiskCache{} },
	Version: opencodeCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})
