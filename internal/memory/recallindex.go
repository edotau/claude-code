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
	empty := func() map[fileKey]indexed { return make(map[fileKey]indexed) }
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 28 {
		return empty()
	}
	u32 := func(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
	u64 := func(b []byte) int64 { return int64(binary.LittleEndian.Uint64(b)) }
	if string(data[:4]) != recallIndexMagic || u32(data[4:]) != recallIndexVersion ||
		u64(data[8:]) != want.size || u64(data[16:]) != want.mtimeNs {
		return empty()
	}
	count := u32(data[24:])
	data = data[28:]
	m := empty()
	for i := uint32(0); i < count; i++ {
		if len(data) < 4 {
			return empty()
		}
		length := uint64(u32(data))
		data = data[4:]
		if length+21 > uint64(len(data)) {
			return empty()
		}
		key := fileKey{string(data[:length]), segMode(data[length])}
		data = data[length+1:]
		entry := indexed{size: u64(data), mtimeNs: u64(data[8:])}
		length = uint64(u32(data[16:]))
		data = data[20:]
		if length > uint64(len(data)) {
			return empty()
		}
		entry.seg = segView(data[:length])
		if key.mode > segKnowledge || !entry.seg.valid() {
			return empty()
		}
		m[key] = entry
		data = data[length:]
	}
	if len(data) != 0 {
		return empty()
	}
	return m
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
		info, err := os.Stat(f.path)
		stats[f.path] = err == nil
		if err != nil {
			continue
		}
		key := fileKey{f.path, f.mode}
		cached, found := index[key]
		if found && cached.size == info.Size() && cached.mtimeNs == info.ModTime().UnixNano() {
			segs[i] = cached.seg
			continue
		}
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		segs[i] = encodeSegment(parseBlocks(f.mode, filepath.Base(f.path), data))
		if persist && ok && time.Since(info.ModTime()) >= racyWindow {
			index[key] = indexed{info.Size(), info.ModTime().UnixNano(), segs[i]}
			dirty = true
		}
	}
	if dirty {
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
		_ = writeRecallIndex(path, stamp, index)
	}
	return segs
}
