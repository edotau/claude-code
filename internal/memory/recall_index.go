package memory

import (
	"encoding/binary"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

const (
	recallIndexFile           = "recall.idx"
	recallIndexMagic          = "CCRI"
	recallIndexVersion uint32 = 1
	racyWindow                = 2 * time.Second
)

type exeStamp struct{ size, mtimeNs int64 }
type fileKey struct {
	path string
	mode segMode
}
type indexed struct {
	size, mtimeNs int64
	seg           segView
}

func currentExeStamp() (exeStamp, bool) {
	path, err := os.Executable()
	if err != nil {
		return exeStamp{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return exeStamp{}, false
	}
	return exeStamp{info.Size(), info.ModTime().UnixNano()}, true
}

// readRecallIndex decodes the entries in want, or every entry when want is nil; invalid entries are left out.
func readRecallIndex(path string, stamp exeStamp, want map[fileKey]bool) map[fileKey]indexed {
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 28 {
		return make(map[fileKey]indexed)
	}
	u32 := func(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
	u64 := func(b []byte) int64 { return int64(binary.LittleEndian.Uint64(b)) }
	if string(data[:4]) != recallIndexMagic || u32(data[4:]) != recallIndexVersion ||
		u64(data[8:]) != stamp.size || u64(data[16:]) != stamp.mtimeNs {
		return make(map[fileKey]indexed)
	}
	m, ok := readIndexEntries(data[28:], u32(data[24:]), want)
	if !ok {
		return make(map[fileKey]indexed)
	}
	return m
}

// readIndexEntries decodes count entries from data; false when any is mis-framed or trailing bytes remain.
// Entries outside want are skipped unvalidated, and a wanted entry with an invalid segment is a miss.
func readIndexEntries(data []byte, count uint32, want map[fileKey]bool) (map[fileKey]indexed, bool) {
	m := make(map[fileKey]indexed)
	for range count {
		key, entry, rest, ok := readIndexEntry(data)
		if !ok {
			return nil, false
		}
		data = rest
		if want != nil && !want[key] || key.mode > segKnowledge || !entry.seg.Valid() {
			continue
		}
		m[key] = entry
	}
	return m, len(data) == 0
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

func writeRecallIndex(path string, stamp exeStamp, m map[fileKey]indexed) error {
	keys := make([]fileKey, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].path != keys[j].path {
			return keys[i].path < keys[j].path
		}
		return keys[i].mode < keys[j].mode
	})
	data := append([]byte{}, recallIndexMagic...)
	data = binary.LittleEndian.AppendUint32(data, recallIndexVersion)
	data = binary.LittleEndian.AppendUint64(data, uint64(stamp.size))
	data = binary.LittleEndian.AppendUint64(data, uint64(stamp.mtimeNs))
	data = binary.LittleEndian.AppendUint32(data, uint32(len(keys)))
	for _, key := range keys {
		entry := m[key]
		data = binary.LittleEndian.AppendUint32(data, uint32(len(key.path)))
		data = append(data, key.path...)
		data = append(data, byte(key.mode))
		data = binary.LittleEndian.AppendUint64(data, uint64(entry.size))
		data = binary.LittleEndian.AppendUint64(data, uint64(entry.mtimeNs))
		data = binary.LittleEndian.AppendUint32(data, uint32(len(entry.seg)))
		data = append(data, entry.seg...)
	}
	return paths.AtomicWrite(path, data, 0o600)
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

func loadSegments(files []corpusFile, persist bool) []segView {
	stamp, ok := currentExeStamp()
	path := filepath.Join(StateDir(), recallIndexFile)
	var index map[fileKey]indexed
	if ok {
		want := make(map[fileKey]bool, len(files))
		for _, f := range files {
			want[fileKey{f.Path, f.Mode}] = true
		}
		index = readRecallIndex(path, stamp, want)
	}
	segs := make([]segView, len(files))
	// Reuse query stats when pruning so each corpus file is statted only once.
	stats := make(map[string]bool, len(files))
	added := map[fileKey]indexed{}
	for i, f := range files {
		info, err := os.Stat(f.Path)
		stats[f.Path] = err == nil
		if err != nil {
			continue
		}
		key := fileKey{f.Path, f.Mode}
		if seg, hit := index[key].fresh(info); hit {
			segs[i] = seg
			continue
		}
		segs[i] = encodeFile(f)
		if segs[i] != nil && persist && ok && time.Since(info.ModTime()) >= racyWindow {
			added[key] = indexed{info.Size(), info.ModTime().UnixNano(), segs[i]}
		}
	}
	if len(added) > 0 && os.MkdirAll(filepath.Dir(path), 0o755) == nil { // re-read under the lock: keep peers' entries
		_, _ = paths.TryFileLock(path+".lock", time.Second, func() error {
			cur := readRecallIndex(path, stamp, nil)
			maps.Copy(cur, added)
			pruneMissing(cur, stats)
			return writeRecallIndex(path, stamp, cur)
		})
	}
	return segs
}
