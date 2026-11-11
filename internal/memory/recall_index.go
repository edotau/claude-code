package memory

import (
	"encoding/binary"
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

func readRecallIndex(path string, want exeStamp) map[fileKey]indexed {
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 28 {
		return make(map[fileKey]indexed)
	}
	u32 := func(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
	u64 := func(b []byte) int64 { return int64(binary.LittleEndian.Uint64(b)) }
	if string(data[:4]) != recallIndexMagic || u32(data[4:]) != recallIndexVersion ||
		u64(data[8:]) != want.size || u64(data[16:]) != want.mtimeNs {
		return make(map[fileKey]indexed)
	}
	m, ok := readIndexEntries(data[28:], u32(data[24:]))
	if !ok {
		return make(map[fileKey]indexed)
	}
	return m
}

// readIndexEntries decodes count entries from data; false when any is short, invalid or trailing bytes remain.
func readIndexEntries(data []byte, count uint32) (map[fileKey]indexed, bool) {
	m := make(map[fileKey]indexed)
	for range count {
		key, entry, rest, ok := readIndexEntry(data)
		if !ok {
			return nil, false
		}
		m[key] = entry
		data = rest
	}
	return m, len(data) == 0
}

// readIndexEntry decodes one entry from the front of data and returns the bytes after it.
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
	if key.mode > segKnowledge || !entry.seg.Valid() {
		return key, entry, nil, false
	}
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
		index = readRecallIndex(path, stamp)
	}
	segs := make([]segView, len(files))
	// Reuse query stats when pruning so each corpus file is statted only once.
	stats := make(map[string]bool, len(files))
	dirty := false
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
			index[key] = indexed{info.Size(), info.ModTime().UnixNano(), segs[i]}
			dirty = true
		}
	}
	if dirty {
		pruneMissing(index, stats)
		_ = writeRecallIndex(path, stamp, index)
	}
	return segs
}
