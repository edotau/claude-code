package memory

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/textutil"
)

// plansOldPeer is the probe's plans line when the peer's binary lacks `memory import --plans`.
const plansOldPeer = "old"

// plansProbeScript prints the peer plans home's digest, "none" when missing, plansOldPeer on an old binary.
func plansProbeScript() string {
	return `{ ` + remote.PathPrefix + ` ` + textutil.QuotePath(remote.HomePath(remote.Bin())) +
		` memory import -h 2>&1 | grep -q -e --plans && { cd "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/plans" 2>/dev/null && ` +
		`find . -type f ! -name '.*' ! -name '*.lock' -exec cksum {} + | LC_ALL=C sort | cksum || echo none; } || echo ` + plansOldPeer + `; }`
}

const plansFetchScript = `p="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/plans"; mkdir -p "$p" && cd "$p" && ` +
	`COPYFILE_DISABLE=1 tar -cf - --exclude='*.lock' .`

func plansRoot() string { return filepath.Join(paths.ConfigDir(), "plans") }

// plansWrite is one side's changes: rels to copy in; over and drop hold the hash a file must still have.
type plansWrite struct {
	rels       []string
	over, drop map[string]string
}

func newPlansWrite() plansWrite {
	return plansWrite{over: map[string]string{}, drop: map[string]string{}}
}

// plansAction is one peer's plans decisions: take applies here, send on the peer.
type plansAction struct {
	take, send plansWrite
	differ     int
}

// decidePlans applies DecideSync per file; a plan archived in the store leaves every side whose copy is unedited.
func decidePlans(seen, local, theirs, archived map[string]string) plansAction {
	a := plansAction{take: newPlansWrite(), send: newPlansWrite()}
	for _, rel := range slices.Sorted(maps.Keys(union(local, theirs))) {
		l, t := local[rel], theirs[rel]
		switch {
		case archived[rel] != "" && a.archived(rel, seen[rel], l, t, archived[rel]):
		case l == "":
			a.take.rels = append(a.take.rels, rel) // deletions do not propagate: a file only the peer has comes back
		case t == "":
			a.send.rels = append(a.send.rels, rel)
		default:
			a.decide(rel, seen[rel], l, t)
		}
	}
	return a
}

func (a *plansAction) decide(rel, seen, l, t string) {
	switch DecideSync(seen, l, t) {
	case SyncTakePeer:
		a.take.rels, a.take.over[rel] = append(a.take.rels, rel), l
	case SyncSendLocal:
		a.send.rels, a.send.over[rel] = append(a.send.rels, rel), t
	case SyncDiffer, SyncStanding:
		a.differ++
	}
}

// archived drops a copy equal to the archive, or unedited since sync once the other side moved it; false: none dropped.
func (a *plansAction) archived(rel, seen, l, t, h string) bool {
	ls, ts := seenPair(seen) // a reused name both sides still hold never matches, so a live plan syncs as usual
	if l != "" && (l == h || l == ls && t == "") {
		a.take.drop[rel] = l
	}
	if t != "" && (t == h || t == ts && l == "") {
		a.send.drop[rel] = t
	}
	return a.take.drop[rel] != "" || a.send.drop[rel] != ""
}

// seenPair splits a seen value into its local and peer hashes; one hash stands for both.
func seenPair(seen string) (string, string) {
	f := strings.Fields(seen)
	if len(f) == 0 {
		return "", ""
	}
	return f[0], f[len(f)-1]
}

func union(a, b map[string]string) map[string]string {
	out := maps.Clone(a)
	maps.Copy(out, b)
	return out
}

// archivedPlans hashes each plan's store archive copy: plans/X is archive/plans/claude/X, else archive/plans/X.
func archivedPlans(rels map[string]string) map[string]string {
	out := map[string]string{}
	root, err := os.OpenRoot(filepath.Join(paths.MemoryStoreDir(), "archive", "plans"))
	if err != nil {
		return out
	}
	defer root.Close()
	for rel := range rels {
		for _, arch := range []string{"claude/" + rel, rel} { // the top-level home's slug, then plans/<slug>/X.md's own
			if h, ok := hashFile(root, arch); ok {
				out[rel] = h
				break
			}
		}
	}
	return out
}

// settlePlans records what both sides hold after the pass; a both-sides change keeps its pair, a gone file is dropped.
func settlePlans(seen, local, peer map[string]string) {
	for rel := range union(seen, union(local, peer)) {
		l, t := local[rel], peer[rel]
		switch {
		case l == "" && t == "":
			delete(seen, rel)
		case l == t:
			seen[rel] = l
		case l != "" && t != "" && DecideSync(seen[rel], l, t) == SyncDiffer:
			seen[rel] = l + " " + t
		}
	}
}

// plans syncs the plans home with one peer after the memory pass; a failure is reported and never fails the pass.
func (s *syncPass) plans(name string, p *PeerSync) {
	peerTok, local := s.plansTok[name], hashTree(plansRoot())
	if peerTok == p.PlansPulled && treeDigest(local) == p.PlansPushed {
		s.say("plans", name, "unchanged")
		return
	}
	dir, err := fetchTar(name, plansFetchScript)
	defer func() { _ = os.RemoveAll(dir) }()
	if err != nil {
		s.say("plans", name, "fetch failed: "+err.Error()+"; retried at the next pass")
		return
	}
	theirs := hashTree(dir)
	a := decidePlans(p.PlansSeen, local, theirs, archivedPlans(union(local, theirs)))
	lines, ok := s.takePlans(name, p, dir, a.take, theirs)
	if !ok {
		return
	}
	kept, ok := s.sendPlans(name, a)
	if !ok || s.dryRun {
		return
	}
	local = plansSent(p.PlansSeen, a, theirs, kept)
	if len(kept) == 0 && !slices.ContainsFunc(lines, isKeptLine) { // a held-back file keeps the next pass looking
		p.PlansPulled, p.PlansPushed = peerTok, treeDigest(local)
	}
}

// takePlans applies the take side here and settles it, so a failed send never turns a taken file into a differ.
func (s *syncPass) takePlans(name string, p *PeerSync, dir string, take plansWrite, theirs map[string]string) ([]string, bool) {
	lines, err := applyPlans(plansRoot(), dir, take, s.dryRun)
	for _, l := range lines {
		s.say("plans", name, l)
	}
	if err != nil {
		s.say("plans", name, "writing here: "+err.Error()+"; retried at the next pass")
		return nil, false
	}
	if !s.dryRun {
		settlePlans(p.PlansSeen, hashTree(plansRoot()), theirs)
	}
	return lines, true
}

// plansSent settles seen against what the peer now holds and returns the local tree.
func plansSent(seen map[string]string, a plansAction, theirs map[string]string, kept map[string]bool) map[string]string {
	local, peer := hashTree(plansRoot()), maps.Clone(theirs)
	for _, rel := range a.send.rels {
		if !kept[rel] {
			peer[rel] = local[rel]
		}
	}
	for rel := range a.send.drop {
		if !kept[rel] {
			delete(peer, rel)
		}
	}
	settlePlans(seen, local, peer)
	return local
}

func isKeptLine(l string) bool { _, ok := keptFile(l); return ok }

// sendPlans pushes the send side; kept names the files the peer or the credential check held back.
func (s *syncPass) sendPlans(name string, a plansAction) (map[string]bool, bool) {
	kept := map[string]bool{}
	if len(a.send.rels)+len(a.send.drop) == 0 {
		return kept, true
	}
	var in, out bytes.Buffer
	if err := writePlansTar(&in, a.send, kept); err != nil {
		s.say("plans", name, "packing: "+err.Error())
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), memoryFetchTimeout)
	defer cancel()
	tail, exit, err := syncSSH(ctx, name, plansPushScript(s.dryRun), &in, &out)
	switch {
	case err != nil || exit == sshFailExit || exit < 0:
		s.say("plans", name, "connection lost: "+cmpErr(err, tail)+"; retried at the next pass")
	case exit == 2 && strings.Contains(tail, "-plans"):
		s.say("plans", name, "peer's claude-code predates plans sync (update it); plans not sent, memory unaffected")
	case exit != 0:
		s.say("plans", name, fmt.Sprintf("send failed (exit %d): %s", exit, lastLine(tail)))
	default:
		s.say("plans", name, fmt.Sprintf("sent %d, drop %d: %s", len(a.send.rels), len(a.send.drop), readPlansAnswer(out.String(), kept)))
		return kept, true
	}
	return nil, false
}

// readPlansAnswer marks each file the peer kept or skipped and returns its summary line.
func readPlansAnswer(out string, kept map[string]bool) string {
	answer := strings.Split(strings.TrimSpace(out), "\n")
	for _, l := range answer {
		if rel, ok := keptFile(l); ok {
			kept[rel] = true
		}
	}
	return answer[len(answer)-1]
}

func plansPushScript(dryRun bool) string {
	imp := textutil.QuotePath(remote.HomePath(remote.Bin())) + ` memory import --plans --from "$d"`
	if dryRun {
		imp += " --dry-run"
	}
	return `d=$(mktemp -d) || exit 1; trap 'rm -rf "$d"' EXIT; tar -xf - -C "$d" || exit 1; ` + remote.PathPrefix + " " + imp
}

// writePlansTar writes the .replace and .remove lists, then the plans; one it cannot send is marked kept.
func writePlansTar(w io.Writer, pw plansWrite, kept map[string]bool) error {
	root, err := os.OpenRoot(plansRoot())
	if err != nil {
		return err
	}
	defer root.Close()
	tw := tar.NewWriter(w)
	if err := addTarBytes(tw, ".replace", hashList(pw.over)); err != nil {
		return err
	}
	if err := addTarBytes(tw, ".remove", hashList(pw.drop)); err != nil {
		return err
	}
	for _, rel := range pw.rels {
		data, err := readSource(root, rel)
		if err != nil || credentialShaped(data) {
			kept[rel] = true
			continue
		}
		if err := addTarBytes(tw, rel, data); err != nil {
			return err
		}
	}
	return tw.Close()
}

func addTarBytes(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// hashList is the --replace format, "<sha256>  <rel>" per line, sorted.
func hashList(m map[string]string) []byte {
	var b strings.Builder
	for _, rel := range slices.Sorted(maps.Keys(m)) {
		fmt.Fprintf(&b, "%s  %s\n", m[rel], rel)
	}
	return []byte(b.String())
}

func treeDigest(m map[string]string) string { return digestOf(hashList(m)) }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ImportPlans is the peer side of plans sync: from holds the sent plans plus the .replace and .remove lists.
func ImportPlans(from string, dryRun bool) ([]string, error) {
	pw := plansWrite{rels: slices.Sorted(maps.Keys(hashTree(from)))}
	var err error
	if pw.over, err = ReadReplaceList(filepath.Join(from, ".replace")); err != nil {
		return nil, err
	}
	if pw.drop, err = ReadReplaceList(filepath.Join(from, ".remove")); err != nil {
		return nil, err
	}
	lines, err := applyPlans(plansRoot(), from, pw, dryRun)
	n := func(p string) int { return countPrefix(lines, p) }
	return append(lines, fmt.Sprintf("plans: %d written, %d removed, %d kept", n("write "), n("remove "),
		n("kept local ")+n("skipped "))), err
}

// applyPlans copies pw.rels into home and drops pw.drop's files while each hash matches, under the plans lock.
func applyPlans(home, from string, pw plansWrite, dryRun bool) (lines []string, err error) {
	err = plans.WithLock(func() error {
		lines, err = applyPlansLocked(home, from, pw, dryRun)
		return err
	})
	return lines, err
}

func applyPlansLocked(home, from string, pw plansWrite, dryRun bool) ([]string, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	dst, err := os.OpenRoot(home)
	if err != nil {
		return nil, err
	}
	defer dst.Close()
	src, err := os.OpenRoot(from)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	var lines []string
	for _, rel := range pw.rels {
		l, err := applyPlan(dst, src, rel, pw.over[rel], dryRun)
		if err != nil {
			return lines, err
		}
		lines = append(lines, l)
	}
	for _, rel := range slices.Sorted(maps.Keys(pw.drop)) {
		lines = append(lines, dropPlan(dst, rel, pw.drop[rel], dryRun))
	}
	return slices.DeleteFunc(lines, func(l string) bool { return l == "" }), nil
}

// applyPlan writes one plan unless the local file is equal, or differs from want (an edit the sync must keep).
func applyPlan(dst, src *os.Root, rel, want string, dryRun bool) (string, error) {
	data, err := readSource(src, rel)
	switch {
	case err != nil:
		return "skipped " + rel + ": " + err.Error(), nil
	case credentialShaped(data):
		return "skipped " + rel + ": " + credentialMark, nil
	}
	name := filepath.FromSlash(rel)
	if fi, err := dst.Lstat(name); err == nil {
		cur, _ := hashFile(dst, rel)
		switch {
		case !fi.Mode().IsRegular():
			return "skipped " + rel + ": not a regular file", nil
		case cur == digestOf(data):
			return "", nil
		case want == "" || cur != want:
			return "kept local " + rel, nil
		}
	}
	if dryRun {
		return "write " + rel, nil
	}
	if err := dst.MkdirAll(path.Dir(rel), 0o700); err != nil {
		return "", err
	}
	return "write " + rel, writeAtomic(dst, name, data)
}

// writeAtomic writes through a dot temp file hashTree skips, so lock-free readers never see a partial plan.
func writeAtomic(dst *os.Root, name string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(name), ".sync-"+filepath.Base(name))
	if err := dst.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := dst.Rename(tmp, name); err != nil {
		return errors.Join(err, dst.Remove(tmp))
	}
	return nil
}

// dropPlan removes an archived plan whose hash is still want; an edited copy is kept.
func dropPlan(dst *os.Root, rel, want string, dryRun bool) string {
	cur, ok := hashFile(dst, rel)
	switch {
	case !ok:
		return ""
	case cur != want:
		return "kept local " + rel
	case !dryRun:
		if err := dst.Remove(filepath.FromSlash(rel)); err != nil {
			return "skipped " + rel + ": " + err.Error()
		}
	}
	return "remove " + rel
}
