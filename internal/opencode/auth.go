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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
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
	bin, err := paths.AgentBin("opencode")
	if err != nil {
		return nil, err
	}
	login := "opencode" // `auth list` reports OPENCODE_API_KEY from env; `--version` needs no login
	if len(args) > 0 && args[0] == "--version" {
		login = ""
	}
	return agent.ProbeCLI(agent.ProbeContext(ctx, login), bin, agent.EnvSpec{Mode: agent.EnvAgent}, false, args...)
}

var (
	opencodeMu        sync.Mutex
	opencodeMajorKey  string
	opencodeMajorVal  int
	opencodeMajorSet  bool
	opencodeAuthKey   string
	opencodeAuthVal   []OpencodeIntegration
	opencodeAuthErr   error
	opencodeAuthAt    time.Time // when a failed probe was stored; it is retried after opencodeAuthRetry
	opencodeAuthIsSet bool
	opencodeAuthGen   int // bumped by InvalidateOpencodeAuth so an in-flight probe cannot store a stale answer
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

// OpencodeMajor is the major version from `opencode --version` (cached per binary); 0 = absent, unparseable or unprobed.
func OpencodeMajor() int {
	v, _ := opencodeMajor()
	return v
}

// opencodeMajor is OpencodeMajor plus the probe error (never cached); a major-0 answer stays in memory, not on disk.
func opencodeMajor() (int, error) {
	key := opencodeBinKey()
	opencodeMu.Lock()
	if opencodeMajorSet && opencodeMajorKey == key {
		defer opencodeMu.Unlock()
		return opencodeMajorVal, nil
	}
	opencodeMu.Unlock()
	path, _ := paths.AgentBin("opencode") // "" when absent: the probe reports why
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
	v := textutil.VersionMajor(line)
	storeOpencodeMajor(key, v)
	return v, nil
}

func storeOpencodeMajor(key string, v int) {
	opencodeMu.Lock()
	opencodeMajorKey, opencodeMajorVal, opencodeMajorSet = key, v, true
	opencodeMu.Unlock()
}

// OpencodeAuth lists opencode's stored logins, sorted by integration ID: 2.x via `opencode auth list --format
// json --standalone`, 1.x via auth.json. A failed 2.x or version probe still returns the auth.json data, plus an
// error. Cached per binary/auth.json state, a failure for opencodeAuthRetry (a version-probe failure not at all).
func OpencodeAuth() ([]OpencodeIntegration, error) {
	bin, file, key := opencodeAuthKeys()
	opencodeMu.Lock()
	fresh := opencodeAuthErr == nil || time.Since(opencodeAuthAt) < opencodeAuthRetry
	if opencodeAuthIsSet && opencodeAuthKey == key && fresh {
		defer opencodeMu.Unlock()
		return opencodeAuthVal, opencodeAuthErr
	}
	gen := opencodeAuthGen
	opencodeMu.Unlock()
	major, merr := opencodeMajor()
	var ints []OpencodeIntegration
	var err error
	if major >= 2 {
		ints, err = opencodeAuthWithFallback(bin, file)
	} else {
		ints, err = opencodeAuthV1(file)
		if merr != nil && bin != "" { // version unknown, so these 1.x files may not be the login store
			return ints, errors.Join(merr, err)
		}
	}
	opencodeMu.Lock()
	if gen == opencodeAuthGen {
		opencodeAuthKey, opencodeAuthVal, opencodeAuthErr, opencodeAuthAt, opencodeAuthIsSet = key, ints, err, time.Now(), true
	}
	opencodeMu.Unlock()
	return ints, err
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
	opencodeMu.Lock()
	opencodeAuthIsSet, opencodeAuthVal, opencodeAuthErr = false, nil, nil
	opencodeAuthGen++
	ownLogin.Drop()
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

// OpencodeLogins lists the integration IDs opencode has at least one connection for (any version), sorted.
func OpencodeLogins() []string {
	names, _ := opencodeLoginsErr()
	return names
}

// opencodeLoginsErr is OpencodeLogins plus the probe error, so a failed `auth list` is not read as logged out.
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

// OpencodeLogoutArgv is the non-interactive logout command: 2.x needs the credential, 1.x only the
// integration. An unresolved bin/opencode (missing or a broken link) is an error, never a bare "opencode"
// PATH guess, so callers report the same "not installed / broken link" message as everywhere else.
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

// updateOpencodeDiskCache read-modify-writes the cache under a file lock (0600 in a 0700 dir); failures are ignored.
func updateOpencodeDiskCache(fn func(*opencodeDiskCache)) {
	file := opencodeCacheFile()
	if os.MkdirAll(filepath.Dir(file), 0o700) != nil {
		return
	}
	_ = paths.WithFileLock(file, func() error {
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
		updateOpencodeDiskCache(func(c *opencodeDiskCache) { c.Auth = &opencodeDiskAuth{Bin: bin, DB: db, Integrations: ints} })
	}
	return ints, err
}
