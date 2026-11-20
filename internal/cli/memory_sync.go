package cli

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// syncDigestScript hashes the dirty paths and each changed file's blob id, with git alone and writing nothing.
const syncDigestScript = `{ git --no-optional-locks status --porcelain; ` +
	`{ git -c core.quotePath=false diff --name-only --diff-filter=d HEAD; git -c core.quotePath=false ls-files -o --exclude-standard; } | ` +
	`git hash-object --stdin-paths; } | git hash-object --stdin`

// syncProbeScript prints the peer's .layout, store HEAD and dirty-state digest, one per line.
const syncProbeScript = `cd "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/memory" && cat .layout && git rev-parse HEAD && ` + syncDigestScript

const (
	syncOffMsg       = "memory sync is off on this machine: claude-code vault env set NETWORK_MEMORY on"
	syncProbeTimeout = 30 * time.Second
	syncRefusalHold  = 24 * time.Hour
	syncNoLogin      = "no login from this machine"
	importBusyExit   = 75  // sysexits EX_TEMPFAIL: the store is busy, retry later; distinct from 1, 2 and ssh's 255
	sshFailExit      = 255 // ssh's own failure: no connection, no login
	sshTailMax       = 2048
	credentialMark   = "credential-shaped content" // the import's refusal, which repeats until the file changes
)

// syncSSH runs script on host with a 5 s connect timeout; tail is ssh's stderr, exit 255 means ssh itself failed.
var syncSSH = func(ctx context.Context, host, script string, in io.Reader, out io.Writer) (string, int, error) {
	argv, err := syncArgv(host, script)
	if err != nil {
		return "", 0, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	proc.OwnGroup(cmd)
	cmd.WaitDelay = proc.ExecWaitDelay
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, &stderr
	err = cmd.Run()
	tail := stderr.String()
	tail = tail[max(0, len(tail)-sshTailMax):]
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return tail, exit.ExitCode(), nil
	}
	return tail, 0, err
}

// syncArgv is the streaming ssh argv with a 5 s connect timeout, so a host that drops packets fails fast.
func syncArgv(host, script string) ([]string, error) {
	argv, err := remote.StreamArgs(host, script)
	if err != nil {
		return nil, err
	}
	return slices.Insert(argv, 1, "-o", "ConnectTimeout=5"), nil
}

var fetchRoot = fetchMemoryRoot

// memorySync is `memory import --sync`: the opt-in first, then the peers (--host, else CLAUDE_REMOTE_HOST).
func memorySync(host string, dryRun bool) int {
	if !memory.SyncOn() {
		return helptext.FailUsage(syncOffMsg)
	}
	peers := []string{host}
	if host == "" {
		peers = providers.RemoteHosts()
	}
	for i := range peers {
		peers[i] = strings.TrimSpace(peers[i])
	}
	if len(peers) == 0 {
		return helptext.FailUsage("memory import --sync: CLAUDE_REMOTE_HOST names no peer; give --host")
	}
	return runSync(peers, dryRun, os.Stdout)
}

// runSync is one pass under the try-once sync lock: probe every peer, pull from each, then push to each.
func runSync(peers []string, dryRun bool, out io.Writer) int {
	for _, p := range peers {
		if err := textutil.ValidHost(p); err != nil {
			fmt.Fprintf(out, "memory sync: %v; nothing was sent\n", err)
			return 2
		}
	}
	if err := os.MkdirAll(paths.MemoryStateDir(), 0o700); err != nil {
		return helptext.Fail("memory sync: %v", err)
	}
	code := 0
	ran, err := paths.TryFileLock(filepath.Join(paths.MemoryStateDir(), "sync.lock"), 0, func() error {
		code = syncPeers(peers, dryRun, out)
		return nil
	})
	switch {
	case err != nil:
		return helptext.Fail("memory sync: %v", err)
	case !ran:
		fmt.Fprintln(out, "memory sync: another pass is running")
	}
	return code
}

// syncPass is one pass's state; refused counts peers with a refusal, failed is a peer set that is not converged.
type syncPass struct {
	st                   *memory.SyncState
	dryRun               bool
	now                  time.Time
	out                  io.Writer
	layout               string
	synced, down, failed int
}

func syncPeers(peers []string, dryRun bool, out io.Writer) int {
	layout, _ := os.ReadFile(filepath.Join(paths.MemoryStoreDir(), ".layout"))
	s := &syncPass{st: memory.LoadSync(), dryRun: dryRun, now: time.Now(), out: out, layout: strings.TrimSpace(string(layout))}
	tokens := map[string]string{}
	for _, peer := range peers {
		if tok, ok := s.probe(peer, s.peer(peer)); ok {
			tokens[peer] = tok
		}
	}
	bad := map[string]bool{}
	s.each(peers, tokens, bad, s.pull) // every pull first, so each push carries what the other peers had
	s.each(peers, tokens, bad, s.push)
	s.failed += len(bad)
	s.synced = len(tokens) - len(bad)
	if !dryRun {
		if err := memory.SaveSync(s.st); err != nil {
			fmt.Fprintf(out, "memory sync: saving state: %v\n", err)
			s.failed++
		}
	}
	fmt.Fprintf(out, "memory sync: %d peer(s): %d synced, %d unreachable, %d refused\n", len(peers), s.synced, s.down, s.failed)
	return min(s.failed, 1)
}

// each runs one direction for every reachable peer, in list order, marking the ones that did not converge.
func (s *syncPass) each(peers []string, tokens map[string]string, bad map[string]bool, fn func(string, *memory.PeerSync, string) bool) {
	for _, peer := range peers {
		if tok, ok := tokens[peer]; ok && !fn(peer, s.peer(peer), tok) {
			bad[peer] = true
		}
	}
}

func (s *syncPass) peer(name string) *memory.PeerSync {
	p := s.st.Peers[name]
	if p == nil {
		p = &memory.PeerSync{}
		s.st.Peers[name] = p
	}
	if p.Seen == nil {
		p.Seen = map[string]string{}
	}
	return p
}

func (s *syncPass) say(dir, peer, msg string) { fmt.Fprintf(s.out, "%s %s: %s\n", dir, peer, msg) }

// held reports a stored refusal still in force: same source token and under a day old.
func (s *syncPass) held(d memory.SyncDir, token string) bool {
	return d.Refusal != "" && d.Token == token && s.now.Sub(d.Since) < syncRefusalHold
}

func (s *syncPass) refuse(d *memory.SyncDir, dir, peer, why, token string) {
	d.Refusal, d.Since, d.Token = why, s.now, token
	s.say(dir, peer, "refused: "+why)
}

// probe returns the peer's token ("<HEAD> <dirty-state digest>"), or false when it cannot sync now.
func (s *syncPass) probe(name string, p *memory.PeerSync) (string, bool) {
	if p.Pull.Refusal == syncNoLogin && s.held(p.Pull, "") {
		s.down++
		s.say("probe", name, "skipped: "+syncNoLogin+" (tried once a day)")
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncProbeTimeout)
	defer cancel()
	var buf bytes.Buffer
	tail, exit, err := syncSSH(ctx, name, syncProbeScript, nil, &buf)
	p.Tried = s.now
	if s.unreachable(name, p, tail, exit, err) {
		return "", false
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if exit != 0 || len(lines) != 3 {
		s.failed++
		s.refuse(&p.Pull, "probe", name, fmt.Sprintf("probe failed (exit %d): %s", exit, lastLine(tail)), "")
		return "", false
	}
	layout, tok := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])+" "+strings.TrimSpace(lines[2])
	if layout != s.layout {
		s.failed++
		s.refuse(&p.Pull, "probe", name, fmt.Sprintf("layout %q differs from local %q", layout, s.layout), tok)
		return "", false
	}
	if r := p.Pull.Refusal; r == syncNoLogin || strings.HasPrefix(r, "layout ") || strings.HasPrefix(r, "probe failed") {
		p.Pull.Refusal = "" // the probe's own refusal is over once a probe passes
	}
	return tok, true
}

// unreachable reports an ssh-level failure (exit 255, no answer): a refused login is held a day, the rest retried.
func (s *syncPass) unreachable(name string, p *memory.PeerSync, tail string, exit int, err error) bool {
	switch {
	case exit == sshFailExit && strings.Contains(tail, "Permission denied"):
		s.refuse(&p.Pull, "probe", name, syncNoLogin, "")
	case err != nil || exit == sshFailExit || exit < 0:
		if p.Unreachable.IsZero() {
			p.Unreachable = s.now
		}
		s.say("probe", name, "unreachable")
	default:
		p.Unreachable = time.Time{}
		return false
	}
	s.down++
	return true
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// pull fetches the peer's store and imports it, letting a peer file win where only the peer changed it.
func (s *syncPass) pull(name string, p *memory.PeerSync, tok string) bool {
	if tok == p.Pulled {
		s.say("pull", name, "unchanged")
		return true
	}
	if s.held(p.Pull, tok) {
		s.say("pull", name, "refused earlier: "+p.Pull.Refusal)
		return false
	}
	dir, err := fetchRoot(name)
	defer func() { _ = os.RemoveAll(dir) }()
	if err != nil {
		s.say("pull", name, "fetch failed: "+err.Error()+"; retried at the next pass")
		return false
	}
	theirs := hashTree(dir)
	res, err := memory.ImportBank(memory.ImportOptions{From: dir, Label: name, DryRun: s.dryRun, Topics: true,
		Replace: takePeer(p.Seen, memory.HashStore(), theirs)})
	if err != nil {
		_, input := errors.AsType[*memory.ImportInputError](err)
		s.fail(&p.Pull, "pull", name, refusalReason(err), input || strings.Contains(err.Error(), credentialMark), tok)
		return false
	}
	n := countPrefix(res.Lines, "conflict ")
	s.say("pull", name, fmt.Sprintf("%s; differ %d", res.Summary, n))
	if !s.dryRun {
		settleSeen(p.Seen, memory.HashStore(), theirs, wroteFiles(res.Lines))
		p.Pulled, p.Pull = tok, memory.SyncDir{OK: s.now, Summary: res.Summary, Differ: n}
	}
	return true
}

// fail files a failed direction: a lasting refusal is held, anything else (busy, cut off) is retried at the next pass.
func (s *syncPass) fail(d *memory.SyncDir, dir, peer, why string, lasting bool, token string) {
	if lasting {
		s.refuse(d, dir, peer, why, token)
		return
	}
	s.say(dir, peer, why+"; retried at the next pass")
}

// takePeer is the pull's replace list: the files only the peer changed since the last sync, keyed to the local hash.
func takePeer(seen, local, theirs map[string]string) map[string]string {
	replace := map[string]string{}
	for rel, h := range theirs {
		if l, ok := local[rel]; ok && !memory.MergesOnImport(rel) && memory.DecideSync(seen[rel], l, h) == memory.SyncTakePeer {
			replace[rel] = l
		}
	}
	return replace
}

// settleSeen records only what the pull proved; a local-only change or an unequal log keeps its pair for the push.
func settleSeen(seen, local, theirs map[string]string, wrote map[string]bool) {
	for rel, h := range theirs {
		l, ok := local[rel]
		switch {
		case wrote[rel]:
			seen[rel] = h // the import wrote the peer's bytes: an edit since is a local change
		case !ok, memory.MergesOnImport(rel) && l != h:
		case l == h:
			seen[rel] = l
		case memory.DecideSync(seen[rel], l, h) == memory.SyncDiffer:
			seen[rel] = l + " " + h // both keep their own; the next one-sided change wins
		}
	}
}

// wroteFiles lists the files an import's lines say it copied or replaced.
func wroteFiles(lines []string) map[string]bool {
	out := map[string]bool{}
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, "copy "); ok {
			out[rest] = true
		} else if rest, ok := strings.CutPrefix(l, "replace "); ok {
			out[rest] = true
		}
	}
	return out
}

func refusalReason(err error) string {
	if errors.Is(err, memory.ErrBankBusy) {
		return "busy"
	}
	return lastLine(strings.SplitN(err.Error(), "\n", 2)[0])
}

func countPrefix(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// hashTree hashes a fetched memory root's regular files the import could take (dotfiles and generated ones left out).
func hashTree(dir string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		switch name := d.Name(); {
		case err != nil:
		case d.IsDir() && (name == ".git" || name == "locks" || name == ".locks"):
			return fs.SkipDir
		case !d.Type().IsRegular(), strings.HasPrefix(name, "."), strings.HasSuffix(name, ".lock"),
			rel == "MEMORY.md", rel == "index.md", rel == "catalog.md":
		default:
			if b, err := os.ReadFile(p); err == nil {
				sum := sha256.Sum256(b)
				out[rel] = hex.EncodeToString(sum[:])
			}
		}
		return nil
	})
	return out
}

// push sends the files whose local hash moved since the peer last saw them; skipped when the local token did not move.
func (s *syncPass) push(name string, p *memory.PeerSync, peerTok string) bool {
	tok := memory.LocalToken()
	if tok != "" && tok == p.Pushed {
		s.say("push", name, "unchanged")
		return true
	}
	if s.held(p.Push, tok) {
		s.say("push", name, "refused earlier: "+p.Push.Refusal)
		return false
	}
	local := memory.HashStore()
	rels := memory.ChangedSince(local, p.Seen)
	if len(rels) == 0 {
		if !s.dryRun {
			p.Pushed = tok
		}
		s.say("push", name, "nothing changed")
		return true
	}
	a, tail, exit, err := s.send(name, rels, pushReplace(p.Seen, local, rels))
	switch {
	case err != nil || exit == sshFailExit || exit < 0:
		s.say("push", name, "connection lost: "+cmpErr(err, tail)+"; retried at the next pass")
		return false
	case exit != 0:
		why := map[bool]string{true: "busy"}[exit == importBusyExit]
		s.fail(&p.Push, "push", name, cmp.Or(why, fmt.Sprintf("exit %d: %s", exit, lastLine(tail))),
			exit == 2 || strings.Contains(tail, credentialMark), tok)
		return false
	case !a.complete:
		s.say("push", name, "the peer's answer ended before its summary; nothing marked as sent, retried at the next pass")
		return false
	}
	s.pushed(name, p, pushResult{tok: tok, peerTok: peerTok, rels: rels, local: local, a: a})
	return true
}

func cmpErr(err error, tail string) string {
	if err != nil {
		return err.Error()
	}
	return lastLine(tail)
}

// pushReplace is the .replace list: "<peer sha256>  <rel>" for each sent file only this side changed.
func pushReplace(seen, local map[string]string, rels []string) string {
	var b strings.Builder
	for _, rel := range rels {
		f := strings.Fields(seen[rel])
		if len(f) > 0 && !memory.MergesOnImport(rel) && memory.DecideSync(seen[rel], local[rel], f[len(f)-1]) == memory.SyncSendLocal {
			fmt.Fprintf(&b, "%s  %s\n", f[len(f)-1], rel)
		}
	}
	return b.String()
}

type pushResult struct {
	tok, peerTok string
	rels         []string
	local        map[string]string
	a            pushAnswer
}

// pushed records a push: each sent file the peer did not name as kept or skipped is equal; a dry run records nothing.
func (s *syncPass) pushed(name string, p *memory.PeerSync, r pushResult) {
	n := len(r.a.kept)
	s.say("push", name, fmt.Sprintf("sent %d file(s): %s; differ %d", len(r.rels), r.a.summary, n))
	if s.dryRun {
		return
	}
	for _, rel := range r.rels {
		if !r.a.kept[rel] {
			p.Seen[rel] = r.local[rel]
		}
	}
	if r.a.pre == r.peerTok && p.Pulled == r.peerTok && r.a.post != "" { // nothing else moved the peer: the next pull skips
		p.Pulled = r.a.post
	}
	p.Pushed, p.Push = r.tok, memory.SyncDir{OK: s.now, Summary: r.a.summary, Differ: n}
}

// pushAnswer is what the driver reads from the peer's import; complete means the summary came right before sync-post.
type pushAnswer struct {
	kept               map[string]bool
	summary, pre, post string
	complete           bool
}

// readPushAnswer parses the whole answer, any line length; a file named in a conflict, kept or skipped line was kept.
func readPushAnswer(out string) pushAnswer {
	a := pushAnswer{kept: map[string]bool{}}
	for l := range strings.SplitSeq(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if rest, ok := strings.CutPrefix(l, "sync-pre "); ok {
			a.pre = rest
		} else if rest, ok := strings.CutPrefix(l, "sync-post "); ok {
			a.post, a.complete = rest, importSummary(a.summary)
		} else if rel, ok := keptFile(l); ok {
			a.kept[rel] = true
		}
		if l != "" && !strings.HasPrefix(l, "sync-") {
			a.summary = l
		}
	}
	return a
}

// keptFile is the file a "conflict ", "kept local " or "skipped " line names.
func keptFile(l string) (string, bool) {
	for _, prefix := range []string{"conflict ", "skipped "} {
		if rest, ok := strings.CutPrefix(l, prefix); ok {
			return strings.SplitN(rest, ": ", 2)[0], true
		}
	}
	return strings.CutPrefix(l, "kept local ")
}

func importSummary(l string) bool {
	return strings.HasPrefix(l, "imported: ") || strings.HasPrefix(l, "would import: ") || strings.HasPrefix(l, "nothing to import")
}

// send streams the tar to the push script; a peer answering 2 to --replace (an older binary) gets it once without.
func (s *syncPass) send(name string, rels []string, replace string) (a pushAnswer, tail string, exit int, err error) {
	for {
		var in, out bytes.Buffer
		if err := writePushTar(&in, replace, rels); err != nil {
			return pushAnswer{}, "", 0, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), memoryFetchTimeout)
		tail, exit, err = syncSSH(ctx, name, pushScript(replace != "", s.dryRun), &in, &out)
		cancel()
		if exit != 2 || replace == "" || err != nil {
			return readPushAnswer(out.String()), tail, exit, err
		}
		s.say("push", name, "peer refused --replace (exit 2); sending once more without it")
		replace = ""
	}
}

// writePushTar writes .replace (when non-empty) then the store files as one archive: Flush pads without a trailer.
func writePushTar(w io.Writer, replace string, rels []string) error {
	if replace != "" {
		tw := tar.NewWriter(w)
		if err := tw.WriteHeader(&tar.Header{Name: ".replace", Mode: 0o600, Size: int64(len(replace)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, replace); err != nil {
			return err
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return memory.WriteStoreTar(w, append(slices.Clone(rels), "index.md")) // index.md: the peer reads only its entries
}

// pushScript unpacks the tar into a temp dir the trap removes, runs the peer's import, and brackets it with tokens.
func pushScript(replace, dryRun bool) string {
	imp := textutil.QuotePath(remote.HomePath(remote.Bin())) + ` memory import --from "$d" --topics`
	if replace {
		imp += ` --replace "$d/.replace"`
	}
	if dryRun {
		imp += " --dry-run"
	}
	tok := `tok() { printf '%s %s %s\n' "$1" "$(git -C "$m" rev-parse HEAD)" "$(cd "$m" && ` + syncDigestScript + `)"; }; `
	return `m="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/memory"; ` + tok + `d=$(mktemp -d) || exit 1; trap 'rm -rf "$d"' EXIT; ` +
		`tar -xf - -C "$d" || exit 1; tok sync-pre; ` + remote.PathPrefix + " " + imp + `; rc=$?; tok sync-post; exit $rc`
}

// readReplaceList parses --replace FILE: `<sha256>  <rel>` lines as sha256sum prints them ("*" marks binary mode).
func readReplaceList(file string) (map[string]string, error) {
	if file == "" {
		return nil, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		sum, rel, ok := strings.Cut(strings.TrimRight(line, "\r"), " ")
		rel = strings.TrimPrefix(strings.TrimLeft(rel, " "), "*")
		if b, err := hex.DecodeString(sum); !ok || err != nil || len(b) != sha256.Size || rel == "" {
			return nil, fmt.Errorf("%s:%d: want `<sha256>  <file>`", file, i+1)
		}
		out[filepath.ToSlash(rel)] = strings.ToLower(sum)
	}
	return out, nil
}
