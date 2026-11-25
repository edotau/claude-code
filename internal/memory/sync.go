package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
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
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	syncEnv       = "NETWORK_MEMORY"
	syncSchema    = 2
	syncEvery     = 30 * time.Minute
	syncStampName = "memory-sync"
)

// remoteHosts is the peer list seam; tests replace it.
var remoteHosts = providers.RemoteHosts

// SyncOn reports this machine's opt-in to memory sync: NETWORK_MEMORY=on, process env then the vault's.
// A sandbox config dir never opts in: it must not reach a peer whatever env it inherited.
func SyncOn() bool {
	if InSandbox() {
		return false
	}
	v := strings.TrimSpace(os.Getenv(syncEnv))
	if v == "" {
		v = strings.TrimSpace(providers.CustomEnv()[syncEnv])
	}
	return strings.EqualFold(v, "on")
}

// SyncState is state/harness/memory/sync.json: what each peer's last passes saw and decided.
type SyncState struct {
	Schema  int                  `json:"schema"`
	Primary string               `json:"primary"` // "self" or a peer host: its version wins conflicts; "" keeps both
	Peers   map[string]*PeerSync `json:"peers"`
}

// PeerSync is one peer's record.
type PeerSync struct {
	Pull        SyncDir           `json:"pull"`
	Push        SyncDir           `json:"push"`
	Pulled      string            `json:"pulled"` // peer token at the last pull
	Pushed      string            `json:"pushed"` // local token at the last push
	Tried       time.Time         `json:"tried"`
	Unreachable time.Time         `json:"unreachable_since"`
	Seen        map[string]string `json:"seen"`         // file -> "local peer" sha256; one hash when equal
	PlansSeen   map[string]string `json:"plans_seen"`   // Seen for the plans home
	PlansPulled string            `json:"plans_pulled"` // peer plans digest at the last settled plans pass
	PlansPushed string            `json:"plans_pushed"` // local plans digest then
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
	New:  func() any { return new(SyncState) }, Migrate: schemas.SameShape, // v2 adds the plans fields
	Keep: "rebuildable cache; a version mismatch is a miss and the next pass reseeds",
})

// loadSync reads the state file; a missing, unreadable or mismatched file is an empty state.
func loadSync() *SyncState {
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

// syncAction is what one file present on both sides needs.
type syncAction int

const (
	syncEqual     syncAction = iota // both sides hold the same bytes
	syncTakePeer                    // only the peer changed: its file replaces the local one
	syncSendLocal                   // only this side changed: its file replaces the peer's
	syncDiffer                      // both changed, or never seen: both keep their own, counted
	syncStanding                    // neither changed since the last sync but unequal: counted, quiet
)

// decideSync applies "the side that changed since the last sync wins"; seen is the stored "local peer" hash pair.
func decideSync(seen, local, peer string) syncAction {
	if local == peer {
		return syncEqual
	}
	f := strings.Fields(seen)
	if len(f) == 0 {
		return syncDiffer
	}
	localSeen, peerSeen := f[0], f[len(f)-1]
	switch localChanged, peerChanged := local != localSeen, peer != peerSeen; {
	case !localChanged && peerChanged:
		return syncTakePeer
	case localChanged && !peerChanged:
		return syncSendLocal
	case localChanged:
		return syncDiffer
	}
	return syncStanding
}

// hashStore is the sha256 of every file an import would consider, keyed by store-relative slash path.
func hashStore() map[string]string {
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

// changedSince lists the files whose local hash differs from the local half of seen, sorted.
func changedSince(local, seen map[string]string) []string {
	var out []string
	for rel, h := range local {
		if f := strings.Fields(seen[rel]); len(f) == 0 || f[0] != h {
			out = append(out, rel)
		}
	}
	slices.Sort(out)
	return out
}

// localToken is "<HEAD> <digest>"; the digest covers the dirty paths and their bytes, "" when git status fails.
func localToken() string {
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

// mergesOnImport reports a log the import merges by block, which never takes the whole-file replace rule.
func mergesOnImport(rel string) bool { return idFiles[rel] != "" || monthlyArchive(rel) }

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
	st := loadSync()
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

// SyncIfDue starts the detached sync pass when this machine opted in, the 30-minute slot is free and there was activity.
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

// syncPrimarySelf names this machine in SyncState.Primary; any other value is a peer host.
const syncPrimarySelf = "self"

// primarySide is which machine wins a sync conflict; none keeps both versions.
type primarySide int

const (
	primaryNone primarySide = iota
	primaryLocal
	primaryPeer
)

func (st *SyncState) primaryOf(peer string) primarySide {
	switch st.Primary {
	case "":
		return primaryNone
	case syncPrimarySelf:
		return primaryLocal
	case peer:
		return primaryPeer
	}
	return primaryNone
}

// SetSyncPrimary persists the primary machine: "self", a peer host, or "" / "none" to clear it.
func SetSyncPrimary(v string) error {
	v = strings.TrimSpace(v)
	switch {
	case strings.EqualFold(v, "none"):
		v = ""
	case v == syncPrimarySelf || v == "":
	default:
		if err := textutil.ValidHost(v); err != nil {
			return err
		}
	}
	st := loadSync()
	st.Primary = v
	return SaveSync(st)
}

// resolve gives a conflict to the primary's side; every other action stands.
func (s primarySide) resolve(a syncAction) syncAction {
	if a != syncDiffer && a != syncStanding {
		return a
	}
	switch s {
	case primaryLocal:
		return syncSendLocal
	case primaryPeer:
		return syncTakePeer
	}
	return a
}

// syncScope is a bank file a sync may delete: a non-log file inside a store folder, never a dotfile or generated one.
func syncScope(rel string) bool {
	parts := strings.Split(rel, "/")
	if !filepath.IsLocal(filepath.FromSlash(rel)) || mergesOnImport(rel) || foldSkips(rel) || rel == layoutFile || rel == catalogFile ||
		rel == nativeIndexFile || len(parts) > 1 && !slices.Contains(flatSubs, parts[0]) {
		return false
	}
	return !slices.ContainsFunc(parts, func(p string) bool { return strings.HasPrefix(p, ".") || p == "locks" })
}

// heldIn reports rel in files, directly or in the folder a loose note is filed into.
func heldIn(rel string, has func(string) bool) bool {
	if has(rel) {
		return true
	}
	f := NoteFolder(rel)
	return f != "" && !strings.Contains(rel, "/") && has(f+"/"+rel)
}

// localGone lists the seen bank files that no longer exist here (a missing hash alone could be an over-cap file).
func localGone(seen map[string]string) []string {
	if _, err := os.Stat(filepath.Join(paths.MemoryStoreDir(), layoutFile)); err != nil {
		return nil // no readable store: nothing here proves a deletion
	}
	gone := func(rel string) bool {
		_, err := os.Lstat(filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(rel)))
		return errors.Is(err, fs.ErrNotExist)
	}
	var out []string
	for rel := range seen {
		if syncScope(rel) && !heldIn(rel, func(r string) bool { return !gone(r) }) {
			out = append(out, rel)
		}
	}
	slices.Sort(out)
	return out
}

// massDelete guards a damaged fetch or store: over half of a large seen set vanishing is no deletion.
func massDelete(n, seen int) bool { return n > 20 && n*2 > seen }

// localDeletes: drop is what the peer removes while it still holds the hash; back is a peer edit that stays.
func localDeletes(seen map[string]string, gone []string, theirs map[string]string, primary primarySide) (drop map[string]string, back []string) {
	drop = map[string]string{}
	for _, rel := range gone {
		localSeen, peerSeen := seenPair(seen[rel])
		cur := peerSeen
		if theirs != nil {
			if !heldIn(rel, func(r string) bool { return theirs[r] != "" }) {
				continue // the peer lacks it too
			}
			cur = theirs[rel]
		}
		switch {
		case cur == "":
		case cur == peerSeen && localSeen == peerSeen || primary == primaryLocal:
			drop[rel] = cur
		default:
			back = append(back, rel)
		}
	}
	if massDelete(len(drop), len(seen)) {
		return map[string]string{}, nil
	}
	return drop, back
}

// peerDeletes lists the local copies to remove (unedited, or any when the peer is primary); kept counts local edits.
func peerDeletes(seen, local, theirs map[string]string, primary primarySide) (remove map[string]string, kept int) {
	remove = map[string]string{}
	for rel := range seen {
		l := local[rel]
		if l == "" || !syncScope(rel) || heldIn(rel, func(r string) bool { return theirs[r] != "" }) {
			continue
		}
		if localSeen, peerSeen := seenPair(seen[rel]); l == localSeen && localSeen == peerSeen || primary == primaryPeer {
			remove[rel] = l
		} else {
			kept++
		}
	}
	if massDelete(len(remove), len(seen)) {
		return map[string]string{}, 0
	}
	return remove, kept
}

// planRemovals plans each sync deletion: a file still holding its hash goes; an edited one stays as a conflict.
func (p *importPlan) planRemovals(remove map[string]string) {
	for _, rel := range slices.Sorted(maps.Keys(remove)) {
		abs := filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(rel))
		fi, err := os.Lstat(abs)
		switch {
		case !syncScope(rel):
			p.skipFile(rel, "not a file a sync removes")
		case errors.Is(err, fs.ErrNotExist):
		case err != nil || !fi.Mode().IsRegular():
			p.skipFile(rel, "not a regular file")
		default:
			cur, err := os.ReadFile(abs)
			if err != nil || textutil.SHA256Hex(cur) != remove[rel] {
				p.record(rel, importAction{kind: impConflict, line: "conflict " + rel + ": kept local (edited since the last sync)"})
				continue
			}
			a := importAction{kind: impRemove, line: "remove " + rel, pre: textutil.SHA256Hex(cur)}
			p.lines = append(p.lines, a.line)
			p.st.removed++
			p.changes = append(p.changes, storeChange{Rel: filepath.FromSlash(rel), Delete: true, Planned: true, PreSHA256: a.pre})
			p.acts = append(p.acts, a)
		}
	}
}

func setOf(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for rel := range m {
		out[rel] = true
	}
	return out
}

// withStanding adds the files whose recorded conflict is still open (both halves unmoved, unequal), sorted.
func withStanding(rels []string, local, seen map[string]string) []string {
	for rel, h := range local {
		if l, p := seenPair(seen[rel]); l == h && p != l && !mergesOnImport(rel) && !slices.Contains(rels, rel) {
			rels = append(rels, rel)
		}
	}
	slices.Sort(rels)
	return rels
}

// forgetRemoved drops the seen entries of files this pass removed here, or that both sides lack.
func forgetRemoved(seen map[string]string, lines, gone []string, theirs map[string]string) {
	for _, l := range lines {
		if rel, ok := strings.CutPrefix(l, "remove "); ok {
			delete(seen, rel)
		}
	}
	for _, rel := range gone {
		if !heldIn(rel, func(r string) bool { return theirs[r] != "" }) {
			delete(seen, rel)
		}
	}
}
