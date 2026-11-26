package memory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"hash/crc32"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/parallel"
	"github.com/edotau/claude-code/internal/paths"
)

const (
	recallIndexFile           = "recall.idx"
	recallIndexMagic          = "CCRI"
	recallIndexVersion uint32 = 2  // bump on any encode or ref change the encoderKey samples cannot see
	indexHeaderLen            = 20 // magic, version, encoder key, entry count
	indexTrailerLen           = 4  // CRC-32C of every byte before it
)

var indexCRC = crc32.MakeTable(crc32.Castagnoli)

// encoderKey fingerprints the encoder's output on samples, so an encoder change misses the index on its own.
var encoderKey = sync.OnceValue(func() uint64 {
	sum := sha256.New()
	for _, c := range []struct {
		mode segMode
		base string
		text string
	}{
		{segEntries, "decisionLog.md", "# Log\n\n## 2026-01-01 — Subagent-Router retry\n" +
			"the and of `fenced` <!-- id:0a1b2c3d -->\n```\n## fenced marker\n```\n\n## héllo wörld 42\nplain body\n"},
		{segEntries, "conventions.md", "# Conventions\n\n- commit small, review focused\n  continued line\n- second rule\n"},
		{segNative, "topic.md", "---\nname: Native Topic\ndescription: router context\n---\nbody text here\n"},
		{segEntries, "progress.md", "# P\n\n## One <!-- id:aaaa0001 --> <!-- id:bbbb0002 -->\n" +
			"text with `tick` and snake_case\n```go\n## not a heading <!-- id:cccc0003 -->\nx := 1\n```\ntail\n"},
		{segKnowledge, "note.md", "---\nname: n\n---\n# Knowledge\nbounded latency\n"},
	} {
		sum.Write(encodeSegment(parseBlocks(c.mode, c.base, []byte(c.text))))
	}
	return binary.LittleEndian.Uint64(sum.Sum(nil))
})

type fileKey struct {
	path string
	mode segMode
}
type indexed struct {
	size, mtimeNs int64
	seg           segView
}

// readRecallIndex maps and decodes the entries in want (all when nil); segments alias it, so release comes last.
func readRecallIndex(path string, key uint64, want map[fileKey]bool) (map[fileKey]indexed, func()) {
	data, release, err := paths.ReadMapped(path)
	if err != nil {
		return make(map[fileKey]indexed), func() {}
	}
	return decodeRecallIndex(data, key, want), release
}

// decodeRecallIndex validates data (magic, version, key, CRC, segments) and decodes it; invalid entries are left out.
func decodeRecallIndex(data []byte, key uint64, want map[fileKey]bool) map[fileKey]indexed {
	if len(data) < indexHeaderLen+indexTrailerLen {
		return make(map[fileKey]indexed)
	}
	u32 := func(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
	body, trailer := data[:len(data)-indexTrailerLen], data[len(data)-indexTrailerLen:]
	if string(body[:4]) != recallIndexMagic || u32(body[4:]) != recallIndexVersion || binary.LittleEndian.Uint64(body[8:]) != key {
		return make(map[fileKey]indexed)
	}
	sumOK := make(chan bool, 1) // the checksum runs beside segment validation; its verdict gates the result
	go func() { sumOK <- crc32.Checksum(body, indexCRC) == u32(trailer) }()
	m, ok := readIndexEntries(body[indexHeaderLen:], u32(body[16:]), want)
	sumGood := <-sumOK // always joined: the goroutine reads the mapping, which may be released once this returns
	if !ok || !sumGood {
		return make(map[fileKey]indexed)
	}
	return m
}

// retained is the one mapping loadSegments keeps for the process, because the segments it hands out alias it.
var retained struct {
	sync.Mutex
	path string
	info os.FileInfo
	data []byte
}

// retainedIndexData maps path once per file version; a replaced index leaves the old mapping alive for live segments.
func retainedIndexData(path string) []byte {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	retained.Lock()
	defer retained.Unlock()
	if retained.path == path && retained.info != nil && os.SameFile(retained.info, info) &&
		retained.info.Size() == info.Size() && retained.info.ModTime().Equal(info.ModTime()) {
		return retained.data
	}
	data, _, err := paths.ReadMapped(path)
	if err != nil {
		return nil
	}
	retained.path, retained.info, retained.data = path, info, data
	return data
}

// readIndexEntries decodes count entries; false on mis-framing or trailing bytes, and a bad wanted segment is a miss.
func readIndexEntries(data []byte, count uint32, want map[fileKey]bool) (map[fileKey]indexed, bool) {
	var keys []fileKey
	var entries []indexed
	for range count {
		key, entry, rest, ok := readIndexEntry(data)
		if !ok {
			return nil, false
		}
		data = rest
		if want != nil && !want[key] || key.mode > segKnowledge {
			continue
		}
		keys = append(keys, key)
		entries = append(entries, entry)
	}
	if len(data) != 0 {
		return nil, false
	}
	valid := make([]bool, len(entries)) // each segment is validated on the pool, one slot per entry
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	forEachIndex(order, func(i int) { valid[i] = entries[i].seg.Valid() })
	m := make(map[fileKey]indexed, len(entries))
	for i, key := range keys {
		if valid[i] {
			m[key] = entries[i]
		}
	}
	return m, true
}

// readIndexEntry frames one entry from the front of data and returns the bytes after it; the segment is not validated.
func readIndexEntry(data []byte) (key fileKey, entry indexed, rest []byte, ok bool) {
	u32 := func(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
	u64 := func(b []byte) int64 { return int64(binary.LittleEndian.Uint64(b)) }
	if len(data) < 4 {
		return key, entry, nil, false
	}
	length := uint64(u32(data))
	data = data[4:]
	if length+21 > uint64(len(data)) {
		return key, entry, nil, false
	}
	key = fileKey{string(data[:length]), segMode(data[length])}
	data = data[length+1:]
	entry = indexed{size: u64(data), mtimeNs: u64(data[8:])}
	length = uint64(u32(data[16:]))
	data = data[20:]
	if length > uint64(len(data)) {
		return key, entry, nil, false
	}
	entry.seg = segView(data[:length])
	return key, entry, data[length:], true
}

func writeRecallIndex(path string, key uint64, m map[fileKey]indexed) error {
	keys := make([]fileKey, 0, len(m))
	size := indexHeaderLen + indexTrailerLen
	for fk, entry := range m {
		keys = append(keys, fk)
		size += 25 + len(fk.path) + len(entry.seg)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].path != keys[j].path {
			return keys[i].path < keys[j].path
		}
		return keys[i].mode < keys[j].mode
	})
	data := append(make([]byte, 0, size), recallIndexMagic...)
	data = binary.LittleEndian.AppendUint32(data, recallIndexVersion)
	data = binary.LittleEndian.AppendUint64(data, key)
	data = binary.LittleEndian.AppendUint32(data, uint32(len(keys)))
	for _, fk := range keys {
		entry := m[fk]
		data = binary.LittleEndian.AppendUint32(data, uint32(len(fk.path)))
		data = append(data, fk.path...)
		data = append(data, byte(fk.mode))
		data = binary.LittleEndian.AppendUint64(data, uint64(entry.size))
		data = binary.LittleEndian.AppendUint64(data, uint64(entry.mtimeNs))
		data = binary.LittleEndian.AppendUint32(data, uint32(len(entry.seg)))
		data = append(data, entry.seg...)
	}
	data = binary.LittleEndian.AppendUint32(data, crc32.Checksum(data, indexCRC))
	return paths.AtomicWriteNoSync(path, data, 0o600) // derived cache: a torn file fails the CRC and rebuilds
}

// pruneMissing drops index entries whose file is gone, reusing stats and statting only the unchecked ones.
func pruneMissing(index map[fileKey]indexed, stats map[string]bool) {
	for key := range index {
		exists, checked := stats[key.path]
		if !checked {
			_, err := os.Stat(key.path)
			exists = err == nil
			stats[key.path] = exists
		}
		if !exists {
			delete(index, key)
		}
	}
}

// fresh returns the cached segment when its recorded size and mtime still match info; the zero entry never matches.
func (e indexed) fresh(info os.FileInfo) (segView, bool) {
	if e.seg == nil || e.size != info.Size() || e.mtimeNs != info.ModTime().UnixNano() {
		return nil, false
	}
	return e.seg, true
}

// encodeFile reads and indexes f; nil when it cannot be read.
func encodeFile(f corpusFile) segView {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return nil
	}
	return encodeSegment(parseBlocks(f.Mode, filepath.Base(f.Path), data))
}

// loadSegments is files' segments from the index; with terms and RowRecall set, stale files come from the row store.
func loadSegments(files []corpusFile, persist bool, terms []string) []segView {
	key := encoderKey()
	path := filepath.Join(paths.MemoryStateDir(), recallIndexFile)
	index := readFilesIndex(path, key, files)
	segs := make([]segView, len(files))
	// Reuse query stats when pruning so each corpus file is statted only once.
	stats := make(map[string]bool, len(files))
	added := map[fileKey]indexed{}
	var missing []int
	var infos []os.FileInfo
	for i, f := range files {
		info, err := os.Stat(f.Path)
		stats[f.Path] = err == nil
		if err != nil {
			if !os.IsNotExist(err) {
				terms = nil // an unstattable file declines the rows: Markdown is the fallback
			}
			continue
		}
		key := fileKey{f.Path, f.Mode}
		if seg, hit := index[key].fresh(info); hit {
			segs[i] = seg
			continue
		}
		missing = append(missing, i)
		infos = append(infos, info)
	}
	encodeMissing(files, missing, segs, terms)
	for j, i := range missing {
		info := infos[j]
		if segs[i] != nil && persist && time.Since(info.ModTime()) >= paths.RacyWindow {
			added[fileKey{files[i].Path, files[i].Mode}] = indexed{info.Size(), info.ModTime().UnixNano(), segs[i]}
		}
	}
	persistIndex(path, key, added, stats)
	return segs
}

const corpusReadWorkers = 4

// encodeMissing indexes the missing files; given terms, those RowRecall renders are parsed from its rows, not read.
func encodeMissing(files []corpusFile, missing []int, segs []segView, terms []string) {
	if len(missing) == 0 {
		return
	}
	var docs map[string][]byte // nil when RowRecall is unset, unasked or declines: every file is read
	if RowRecall != nil && len(terms) > 0 {
		if rows, ok := RowRecall(terms); ok {
			docs = rows
		}
	}
	store := paths.MemoryStoreDir() + string(filepath.Separator)
	_, err := parallel.Map(context.Background(), missing, corpusReadWorkers, func(_ context.Context, i int) (struct{}, error) {
		rel := strings.TrimPrefix(files[i].Path, store)
		if data, fromRows := docs[filepath.ToSlash(rel)]; fromRows {
			segs[i] = encodeSegment(parseBlocks(files[i].Mode, filepath.Base(files[i].Path), data))
		} else {
			segs[i] = encodeFile(files[i])
		}
		return struct{}{}, nil
	})
	parallel.Rethrow(err)
}

// readFilesIndex is the persisted index restricted to files' (path, mode) keys.
func readFilesIndex(path string, key uint64, files []corpusFile) map[fileKey]indexed {
	want := make(map[fileKey]bool, len(files))
	for _, f := range files {
		want[fileKey{f.Path, f.Mode}] = true
	}
	return decodeRecallIndex(retainedIndexData(path), key, want)
}

// persistIndex merges added into the on-disk index and drops files that no longer exist.
func persistIndex(path string, key uint64, added map[fileKey]indexed, stats map[string]bool) {
	if len(added) == 0 || os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	_, _ = paths.TryFileLock(path+".lock", time.Second, func() error { // re-read under the lock: keep peers' entries
		cur, release := readRecallIndex(path, key, nil)
		defer release() // writeRecallIndex copies every segment into its buffer before this runs
		maps.Copy(cur, added)
		pruneMissing(cur, stats)
		return writeRecallIndex(path, key, cur)
	})
}
