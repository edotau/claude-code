package memory

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// SyncPrimarySelf names this machine in SyncState.Primary; any other value is a peer host.
const SyncPrimarySelf = "self"

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
	case SyncPrimarySelf:
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
	case v == SyncPrimarySelf || v == "":
	default:
		if err := textutil.ValidHost(v); err != nil {
			return err
		}
	}
	st := LoadSync()
	st.Primary = v
	return SaveSync(st)
}

// resolve gives a conflict to the primary's side; every other action stands.
func (s primarySide) resolve(a SyncAction) SyncAction {
	if a != SyncDiffer && a != SyncStanding {
		return a
	}
	switch s {
	case primaryLocal:
		return SyncSendLocal
	case primaryPeer:
		return SyncTakePeer
	}
	return a
}

// syncScope is a bank file a sync may delete: a non-log file inside a store folder, never a dotfile or generated one.
func syncScope(rel string) bool {
	parts := strings.Split(rel, "/")
	if !filepath.IsLocal(filepath.FromSlash(rel)) || MergesOnImport(rel) || foldSkips(rel) || rel == layoutFile || rel == catalogFile ||
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
			if err != nil || sha256Hex(cur) != remove[rel] {
				p.record(rel, importAction{kind: impConflict, line: "conflict " + rel + ": kept local (edited since the last sync)"})
				continue
			}
			a := importAction{kind: impRemove, line: "remove " + rel, pre: sha256Hex(cur)}
			p.lines = append(p.lines, a.line)
			p.st.removed++
			p.changes = append(p.changes, Change{Rel: filepath.FromSlash(rel), Delete: true, Planned: true, PreSHA256: a.pre})
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
		if l, p := seenPair(seen[rel]); l == h && p != l && !MergesOnImport(rel) && !slices.Contains(rels, rel) {
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
