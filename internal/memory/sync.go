package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

const (
	syncEnv       = "NETWORK_MEMORY"
	syncSchema    = 1
	syncEvery     = 4 * time.Hour
	syncStampName = "memory-sync"
)

// remoteHosts is the peer list seam; tests replace it.
var remoteHosts = providers.RemoteHosts

// SyncOn reports this machine's opt-in to memory sync: NETWORK_MEMORY=on, process env then the vault's.
func SyncOn() bool {
	v := strings.TrimSpace(os.Getenv(syncEnv))
	if v == "" {
		v = strings.TrimSpace(providers.CustomEnv()[syncEnv])
	}
	return strings.EqualFold(v, "on")
}

// SyncState is state/harness/memory/sync.json: what each peer's last passes saw and decided.
type SyncState struct {
	Schema int                  `json:"schema"`
	Peers  map[string]*PeerSync `json:"peers"`
}

// PeerSync is one peer's record.
type PeerSync struct {
	Pull        SyncDir           `json:"pull"`
	Push        SyncDir           `json:"push"`
	Pulled      string            `json:"pulled"` // peer token at the last pull
	Pushed      string            `json:"pushed"` // local token at the last push
	Tried       time.Time         `json:"tried"`
	Unreachable time.Time         `json:"unreachable_since"`
	Seen        map[string]string `json:"seen"` // file -> "local peer" sha256; one hash when equal
}

// SyncDir is the outcome of one direction's last pass.
type SyncDir struct {
	OK      time.Time `json:"ok"`
	Summary string    `json:"summary"`
	Differ  int       `json:"differ"`
	Refusal string    `json:"refusal"`
	Since   time.Time `json:"refused_since"`
	Token   string    `json:"refused_token"`
}

func syncStatePath() string { return filepath.Join(paths.MemoryStateDir(), "sync.json") }

var syncFormat = schemas.Register(schemas.Format{
	Name: "memory-sync", Owner: "internal/memory", Kind: schemas.Cache, Version: syncSchema,
	Glob: syncStatePath,
	New:  func() any { return new(SyncState) },
	Keep: "rebuildable cache; a version mismatch is a miss and the next pass reseeds",
})

// LoadSync reads the state file; a missing, unreadable or mismatched file is an empty state.
func LoadSync() *SyncState {
	st := &SyncState{}
	if b, err := os.ReadFile(syncStatePath()); err != nil || !schemas.DecodeOK(syncFormat, b, st) {
		st = &SyncState{}
	}
	st.Schema = syncSchema
	if st.Peers == nil {
		st.Peers = map[string]*PeerSync{}
	}
	return st
}

// SaveSync writes the state file atomically, 0600.
func SaveSync(st *SyncState) error {
	st.Schema = syncSchema
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.MemoryStateDir(), 0o700); err != nil {
		return err
	}
	return paths.AtomicWrite(syncStatePath(), b, 0o600)
}

// SyncAction is what one file present on both sides needs.
type SyncAction int

const (
	SyncEqual     SyncAction = iota // both sides hold the same bytes
	SyncTakePeer                    // only the peer changed: its file replaces the local one
	SyncSendLocal                   // only this side changed: its file replaces the peer's
	SyncDiffer                      // both changed, or never seen: both keep their own, counted
	SyncStanding                    // neither changed since the last sync but unequal: counted, quiet
)

// DecideSync applies "the side that changed since the last sync wins"; seen is the stored "local peer" hash pair.
func DecideSync(seen, local, peer string) SyncAction {
	if local == peer {
		return SyncEqual
	}
	f := strings.Fields(seen)
	if len(f) == 0 {
		return SyncDiffer
	}
	localSeen, peerSeen := f[0], f[len(f)-1]
	switch localChanged, peerChanged := local != localSeen, peer != peerSeen; {
	case !localChanged && peerChanged:
		return SyncTakePeer
	case localChanged && !peerChanged:
		return SyncSendLocal
	case localChanged:
		return SyncDiffer
	}
	return SyncStanding
}

// HashStore is the sha256 of every file an import would consider, keyed by store-relative slash path.
func HashStore() map[string]string {
	out := map[string]string{}
	root, err := os.OpenRoot(paths.MemoryStoreDir())
	if err != nil {
		return out
	}
	defer root.Close()
	p := &importPlan{}
	_ = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
		case d.IsDir() && skipImportDir(rel, d.Name()):
			return fs.SkipDir
		case d.IsDir(), foldSkips(rel), rel == layoutFile, rel == catalogFile, !p.flatImportFile(rel), !d.Type().IsRegular():
		default:
			if h, ok := hashFile(root, rel); ok {
				out[rel] = h
			}
		}
		return nil
	})
	return out
}

func hashFile(root *os.Root, rel string) (string, bool) {
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return "", false
	}
	defer f.Close()
	h := sha256.New()
	if n, err := io.Copy(h, io.LimitReader(f, importMaxFile+1)); err != nil || n > importMaxFile {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// ChangedSince lists the files whose local hash differs from the local half of seen, sorted.
func ChangedSince(local, seen map[string]string) []string {
	var out []string
	for rel, h := range local {
		if f := strings.Fields(seen[rel]); len(f) == 0 || f[0] != h {
			out = append(out, rel)
		}
	}
	slices.Sort(out)
	return out
}

// LocalToken is "<HEAD> <digest>"; the digest covers the dirty paths and their bytes, "" when git status fails.
func LocalToken() string {
	repo := StoreRepo()
	head, ok := repo.headSHA()
	if !ok {
		var err error
		if head, err = repo.run("rev-parse", "HEAD"); err != nil {
			head = ""
		}
	}
	out, err := repo.run("status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return ""
	}
	h := sha256.New()
	_, _ = io.WriteString(h, out)
	for _, e := range proc.StatusEntries(out) {
		if b, err := os.ReadFile(filepath.Join(repo.Dir, filepath.FromSlash(e.Path))); err == nil {
			sum := sha256.Sum256(b)
			_, _ = h.Write(sum[:])
		}
	}
	return head + " " + hex.EncodeToString(h.Sum(nil))
}

// MergesOnImport reports a log the import merges by block, which never takes the whole-file replace rule.
func MergesOnImport(rel string) bool { return idFiles[rel] != "" || monthlyArchive(rel) }

// SyncRow is one peer's line for the doctor: the last passes, or why there were none.
type SyncRow struct {
	Peer         string
	Pulled       time.Time
	Pushed       time.Time
	Differ       int
	Summary      string
	Unreachable  time.Time
	Refusal      string
	RefusedSince time.Time
}

// SyncReport is one row per peer in CLAUDE_REMOTE_HOST, in list order.
func SyncReport() []SyncRow {
	st := LoadSync()
	var rows []SyncRow
	for _, peer := range remoteHosts() {
		row := SyncRow{Peer: peer}
		if p := st.Peers[peer]; p != nil {
			row.Pulled, row.Pushed, row.Unreachable = p.Pull.OK, p.Push.OK, p.Unreachable
			row.Differ = max(p.Pull.Differ, p.Push.Differ)
			row.Summary = strings.Join(slices.DeleteFunc([]string{p.Pull.Summary, p.Push.Summary}, func(s string) bool { return s == "" }), " / ")
			for _, d := range []SyncDir{p.Pull, p.Push} {
				if d.Refusal != "" {
					row.Refusal, row.RefusedSince = d.Refusal, d.Since
					break
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// SyncIfDue starts the detached sync pass when this machine opted in, the 4-hour slot is free and there was activity.
func SyncIfDue(now, active time.Time, cwd string) bool {
	return SyncOn() && syncIfDue(now, active, cwd)
}

// syncIfDue is SyncIfDue past the opt-in; the idle watcher reads the opt-in once and calls it every poll.
func syncIfDue(now, active time.Time, cwd string) bool {
	if fi, err := os.Stat(filepath.Join(paths.StateDir(), "preflight", syncStampName)); err == nil {
		if now.Sub(fi.ModTime()) < syncEvery || !active.After(fi.ModTime()) {
			return false
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if !catalog.ClaimEvery(syncStampName, syncEvery, now) || len(remoteHosts()) == 0 {
		return false
	}
	return spawn.sync(cwd)
}
