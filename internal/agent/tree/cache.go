// cache.go keeps each subagent transcript's scan, meta and activity on disk so `agents tree` rereads only what changed.

package tree

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

const scanCacheSchema = 1

var usageKey = []byte(`"usage"`)

// respTokens is one deduplicated API response's counts (In = input + cache creation), keyed by message.id.
type respTokens struct {
	ID        string `json:"i"`
	In        int64  `json:"a"`
	Out       int64  `json:"o"`
	CacheRead int64  `json:"r"`
}

// scanEntry is one transcript's scan up to Offset, a line boundary; Size and MtimeNS stamp the file it covered.
type scanEntry struct {
	Size    int64        `json:"size"`
	MtimeNS int64        `json:"mtime_ns"`
	Offset  int64        `json:"offset"`
	Fixed   Tokens       `json:"fixed"` // responses without a message.id, which never deduplicate
	Resp    []respTokens `json:"resp,omitempty"`
	IDs     []string     `json:"dispatch_ids,omitempty"`

	MetaOK    bool         `json:"meta_ok,omitempty"` // Meta and Phase describe the .meta.json stamped below
	MetaSize  int64        `json:"meta_size,omitempty"`
	MetaMtime int64        `json:"meta_mtime_ns,omitempty"`
	Meta      subagentMeta `json:"meta"`
	Phase     string       `json:"phase,omitempty"`

	ActOK  bool      `json:"act_ok,omitempty"` // Act is the tail activity for ActKey at this entry's size and mtime
	ActKey string    `json:"act_key,omitempty"`
	Act    *Activity `json:"act,omitempty"`
}

// scanCache is state/harness/cache/agents-tree-scan.json: transcript path -> scanEntry.
type scanCache struct {
	Schema int                   `json:"schema"`
	Files  map[string]*scanEntry `json:"files"`
	used   map[string]bool
	dirty  bool
}

var scanCacheFormat = schemas.Register(schemas.Format{
	Name: "agents-tree-scan", Owner: "internal/agent/tree", Kind: schemas.Cache,
	Glob:    scanCachePath,
	New:     func() any { return &scanCache{} },
	Version: scanCacheSchema, Keep: "rebuildable cache; a corrupt or newer file is a miss",
})

func scanCachePath() string { return filepath.Join(paths.StateDir(), "cache", "agents-tree-scan.json") }

// loadScanCache reads the cache file; a missing, corrupt or newer one yields an empty cache.
func loadScanCache() *scanCache {
	c := &scanCache{}
	if raw, err := os.ReadFile(scanCachePath()); err != nil || !schemas.DecodeOK(scanCacheFormat, raw, c) {
		c = &scanCache{}
	}
	if c.Files == nil {
		c.Files = map[string]*scanEntry{}
	}
	c.Schema, c.used = scanCacheSchema, map[string]bool{}
	return c
}

// saveScanCache drops entries whose transcript is gone and writes the file when anything changed; best effort.
func saveScanCache(c *scanCache) {
	for p := range c.Files {
		if c.used[p] {
			continue
		}
		if _, err := os.Stat(p); os.IsNotExist(err) {
			delete(c.Files, p)
			c.dirty = true
		}
	}
	if c.dirty {
		_ = paths.WriteJSON(scanCachePath(), c)
	}
}

// totals sums the entry's responses into Tokens.
func (e *scanEntry) totals() Tokens {
	t := e.Fixed
	for _, r := range e.Resp {
		t.add(Tokens{In: r.In, Out: r.Out, CacheRead: r.CacheRead})
	}
	return t
}

// usageRecord is the assistant-line subset the token totals read.
type usageRecord struct {
	Type    string `json:"type"`
	Message struct {
		ID    string `json:"id"`
		Usage *struct {
			Input         int64 `json:"input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			Output        int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// folder folds lines into an entry; the last line per message.id wins, like transcript.ResponseAccumulator.
type folder struct {
	e    *scanEntry
	byID map[string]int
	seen map[string]bool
}

func newFolder(e *scanEntry) *folder {
	f := &folder{e: e, byID: make(map[string]int, len(e.Resp)), seen: make(map[string]bool, len(e.IDs))}
	for i, r := range e.Resp {
		f.byID[r.ID] = i
	}
	for _, id := range e.IDs {
		f.seen[id] = true
	}
	return f
}

func (f *folder) add(line []byte) {
	f.e.IDs = transcript.AppendDispatchIDs(f.e.IDs, f.seen, line)
	if !bytes.Contains(line, usageKey) {
		return
	}
	var rec usageRecord
	if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.Message.Usage == nil {
		return
	}
	u := rec.Message.Usage
	in, out, cr := u.Input+u.CacheCreation, u.Output, u.CacheRead
	if rec.Message.ID == "" {
		f.e.Fixed.add(Tokens{In: in, Out: out, CacheRead: cr})
		return
	}
	r := respTokens{ID: rec.Message.ID, In: in, Out: out, CacheRead: cr}
	if i, ok := f.byID[r.ID]; ok {
		f.e.Resp[i] = r
		return
	}
	f.byID[r.ID] = len(f.e.Resp)
	f.e.Resp = append(f.e.Resp, r)
}

// touch marks the cache for a rewrite; a nil cache (nothing persisted) ignores it.
func (c *scanCache) touch() {
	if c != nil {
		c.dirty = true
	}
}

// scan returns the transcript's entry, resuming at the cached offset; a nil cache or read failure gives a throwaway.
func (c *scanCache) scan(path string) *scanEntry {
	info, err := os.Stat(path)
	if err != nil {
		return &scanEntry{}
	}
	var e *scanEntry
	if c != nil {
		e = c.Files[path]
		c.used[path] = true
	}
	if e != nil && e.Size == info.Size() && e.MtimeNS == info.ModTime().UnixNano() {
		return e
	}
	e = e.resumeFrom(info.Size())
	if err := foldFrom(path, e); err != nil {
		return &scanEntry{IDs: e.IDs}
	}
	e.Size, e.MtimeNS = info.Size(), info.ModTime().UnixNano()
	if c != nil {
		c.Files[path], c.dirty = e, true
	}
	return e
}

// resumeFrom copies e to fold growth into (append-only assumed: a rewritten, larger file is missed), else starts over.
func (e *scanEntry) resumeFrom(size int64) *scanEntry {
	if e == nil || size <= e.Size || e.Offset > size {
		return &scanEntry{}
	}
	return &scanEntry{Offset: e.Offset, Fixed: e.Fixed, Resp: slices.Clone(e.Resp), IDs: slices.Clone(e.IDs)}
}

// foldFrom folds the complete lines after e.Offset into e and advances the offset.
func foldFrom(path string, e *scanEntry) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() // read-only handle: a close error loses nothing
	if _, err := f.Seek(e.Offset, io.SeekStart); err != nil {
		return err
	}
	n, err := textutil.ScanRawLines(f, true, newFolder(e).add)
	e.Offset += n
	return err
}

// meta returns the sidecar meta and workflowPhase, re-reading the .meta.json only when its size or mtime moved.
func (c *scanCache) meta(e *scanEntry, transcriptPath string) (subagentMeta, string) {
	info, err := os.Stat(metaPath(transcriptPath))
	if err != nil {
		return subagentMeta{}, ""
	}
	size, mtime := info.Size(), info.ModTime().UnixNano()
	if e.MetaOK && e.MetaSize == size && e.MetaMtime == mtime {
		return e.Meta, e.Phase
	}
	raw, err := os.ReadFile(metaPath(transcriptPath))
	if err != nil {
		return subagentMeta{}, ""
	}
	e.Meta, e.Phase = parseMeta(raw)
	e.MetaOK, e.MetaSize, e.MetaMtime = true, size, mtime
	c.touch()
	return e.Meta, e.Phase
}

// activity returns lastActivity(path, cwd), cached per cwd and home since both shorten the paths it shows.
func (c *scanCache) activity(e *scanEntry, path, cwd string) Activity {
	home, _ := os.UserHomeDir()
	key := cwd + "\x00" + home
	if e.ActOK && e.ActKey == key {
		if e.Act == nil {
			return Activity{}
		}
		return *e.Act
	}
	act := lastActivity(path, cwd)
	e.ActOK, e.ActKey, e.Act = true, key, &act
	c.touch()
	return act
}
