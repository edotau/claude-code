package shadow

import (
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"
)

// docsOf is a peer's live entries as sorted uid|doc|lines rows: what convergence compares.
func docsOf(t *testing.T, p envPeer) []string {
	t.Helper()
	rows, err := p.db.Query(`SELECT uid, doc, lines FROM entries WHERE deleted = 0 ORDER BY uid`)
	envOK(t, err == nil, "docs: %v", err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u, d, l string
		envOK(t, rows.Scan(&u, &d, &l) == nil, "scan")
		out = append(out, u+"|"+d+"|"+l)
	}
	envOK(t, rows.Err() == nil, "docs: %v", rows.Err())
	return out
}

// meshSync pulls between every ordered pair until a round moves nothing, and fails if it never settles.
func meshSync(t *testing.T, peers ...envPeer) {
	t.Helper()
	for range 6 {
		if meshRound(t, peers) == 0 {
			return
		}
	}
	t.Fatal("mesh did not settle in 6 rounds")
}

// meshRound is one pull per ordered pair; it returns how many envelopes moved.
func meshRound(t *testing.T, peers []envPeer) int {
	t.Helper()
	moved := 0
	for _, from := range peers {
		for _, to := range peers {
			if to.id == from.id {
				continue
			}
			rep, err := pull(from, to)
			envOK(t, err == nil && len(rep.Held) == 0 && len(rep.Errors) == 0, "pull: %+v (%v)", rep, err)
			moved += rep.Applied + rep.Dup + rep.Conflicts
		}
	}
	return moved
}

// pull sends from's changes past to's watermarks over the live serve-export and import-stream path.
func pull(from, to envPeer) (memory.ImportReport, error) {
	since, err := memory.Watermarks(to.st)
	if err != nil {
		return memory.ImportReport{}, err
	}
	var buf bytes.Buffer
	if err := memory.ServeExport(from.st, since, &buf); err != nil {
		return memory.ImportReport{}, err
	}
	body, _, err := memory.ReadStream(&buf)
	if err != nil {
		return memory.ImportReport{}, err
	}
	return memory.ImportStream(to.st, bytes.NewReader(body))
}

func sameDocs(t *testing.T, peers ...envPeer) {
	t.Helper()
	want := docsOf(t, peers[0])
	for i, p := range peers[1:] {
		envOK(t, slices.Equal(want, docsOf(t, p)), "peer %d docs differ:\n%q\n%q", i+1, want, docsOf(t, p))
	}
}

func TestThreePeerFanIn(t *testing.T) {
	a, b, c := newEnvPeer(t), newEnvPeer(t), newEnvPeer(t)
	ua := a.localAdd(t, "notes/a.md", 0, "from a")
	b.localAdd(t, "notes/b.md", 0, "from b")
	c.localAdd(t, "notes/c.md", 0, "from c")
	a.localEdit(t, ua, "from a, edited")
	meshSync(t, a, b, c)
	sameDocs(t, a, b, c)
	envOK(t, meshRound(t, []envPeer{a, b, c}) == 0, "a second round with no new writes moved envelopes")
	envOK(t, len(docsOf(t, c)) == 3 && c.lines(t, ua) == "from a, edited", "fan-in docs = %q", docsOf(t, c))
	for _, p := range []envPeer{a, b, c} {
		w, _, _ := p.st.Watermark(a.id)
		envOK(t, p.id == a.id || w == 2, "watermark of a on %s = %d, want 2", p.id, w)
		envOK(t, p.count(t, `SELECT count(*) FROM conflicts`) == 0, "unexpected conflict rows")
	}
}

func TestThreePeerGapHeldUntilFilled(t *testing.T) {
	a, b, c := newEnvPeer(t), newEnvPeer(t), newEnvPeer(t)
	u := a.localAdd(t, "notes/a.md", 0, "one")
	a.localEdit(t, u, "two")
	a.localEdit(t, u, "three")
	send(t, a, b, a.id, 0)
	rep := send(t, b, c, a.id, 2) // b relays seq 3 only: c has nothing yet
	envOK(t, len(rep.Held) == 1 && rep.Applied == 0 && len(docsOf(t, c)) == 0, "gap relay = %+v", rep)
	rep, err := pull(b, c)
	envOK(t, err == nil && rep.Applied == 3 && len(rep.Held) == 0, "fill gap = %+v (%v)", rep, err)
	envOK(t, c.lines(t, u) == "three", "after fill c has %q", c.lines(t, u))
	w, _, _ := c.st.Watermark(a.id)
	envOK(t, w == 3, "watermark = %d, want 3", w)
	sameDocs(t, a, b, c)
}

func TestThreePeerConflictRow(t *testing.T) {
	a, b, c := newEnvPeer(t), newEnvPeer(t), newEnvPeer(t)
	u := a.localAdd(t, "notes/a.md", 0, "base")
	meshSync(t, a, b, c)
	a.localEdit(t, u, "edit on a")
	b.localEdit(t, u, "edit on b")
	meshSync(t, a, b, c)
	for _, p := range []envPeer{a, b} {
		rows, err := p.st.Conflicts(true)
		envOK(t, err == nil && len(rows) == 1 && rows[0].Kind == conflictDiverged && rows[0].EntryRef == u,
			"conflicts on %s = %+v (%v)", p.id, rows, err)
	}
	envOK(t, a.lines(t, u) == "edit on a" && b.lines(t, u) == "edit on b", "a conflict overwrote a local edit")
	envOK(t, c.count(t, `SELECT count(*) FROM conflicts WHERE resolved = 0`) == 1, "c did not record exactly one conflict")
	rows, _ := b.st.Conflicts(true)
	envOK(t, b.st.ResolveConflict(rows[0].ID, "peer") == nil, "keep peer failed")
	meshSync(t, a, b, c)
	envOK(t, b.lines(t, u) == "edit on a", "b after resolve = %q", b.lines(t, u))
}

func TestThreePeerLateJoiner(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	u := a.localAdd(t, "notes/a.md", 0, "one")
	b.localAdd(t, "notes/b.md", 0, "bee")
	a.localEdit(t, u, "two")
	meshSync(t, a, b)
	c := newEnvPeer(t)
	meshSync(t, a, b, c)
	sameDocs(t, a, b, c)
	envOK(t, len(docsOf(t, c)) == 2 && c.lines(t, u) == "two", "late joiner docs = %q", docsOf(t, c))
	c.localAdd(t, "notes/c.md", 0, "late")
	meshSync(t, a, b, c)
	sameDocs(t, a, b, c)
}

var serveRE = regexp.MustCompile(`memory sync --serve (export|import)(?: --since '([^']*)')?$`)

// pipeSSH is the ssh seam: it runs the peer's --serve side in-process against that host's store, as the CLI does.
func pipeSSH(t *testing.T, peers map[string]envPeer, cut func(export []byte) []byte) func() {
	t.Helper()
	return memory.StubSyncSSH(func(_ context.Context, host, script string, in io.Reader, out io.Writer) (string, int, error) {
		m := serveRE.FindStringSubmatch(script)
		p, ok := peers[host]
		if m == nil || !ok {
			t.Fatalf("unexpected ssh to %q: %q", host, script)
		}
		var err error
		if m[1] == "export" {
			var since map[string]int64
			var buf bytes.Buffer
			if since, err = memory.ParseSince(m[2]); err == nil {
				err = memory.ServeExport(p.st, since, &buf)
			}
			b := buf.Bytes()
			if cut != nil {
				b = cut(b)
			}
			_, _ = out.Write(b)
		} else {
			err = memory.ServeImport(p.st, in, out)
		}
		switch {
		case errors.Is(err, memory.ErrBankBusy):
			return "busy", memory.ImportBusyExit, nil
		case err != nil:
			return err.Error(), 1, nil
		}
		return "", 0, nil
	})
}

func syncOnce(t *testing.T, local envPeer, peer string) (int, string) {
	t.Helper()
	var out strings.Builder
	code := memory.SyncPeers(context.Background(), local.st, []string{peer}, &out)
	return code, out.String()
}

func TestSyncPeersPullsOverSSH(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	defer pipeSSH(t, map[string]envPeer{"a": a}, nil)()
	u := a.localAdd(t, "notes/a.md", 0, "one")
	a.localEdit(t, u, "two")
	code, out := syncOnce(t, b, "a")
	envOK(t, code == 0 && b.lines(t, u) == "two" && strings.Contains(out, "pull a: 2 applied"), "pull: code %d, out %q", code, out)
	w, _, _ := b.st.Watermark(a.id)
	envOK(t, w == 2, "watermark = %d, want 2", w)
	code, out = syncOnce(t, b, "a")
	envOK(t, code == 0 && strings.Contains(out, "pull a: 0 applied") && strings.Contains(out, "push a: 0 applied"), "repeat: code %d, out %q", code, out)
	sameDocs(t, a, b)
}

func TestSyncPeersPushesOverSSH(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	defer pipeSSH(t, map[string]envPeer{"a": a}, nil)()
	u := b.localAdd(t, "notes/b.md", 0, "from b")
	b.localEdit(t, u, "from b, edited")
	code, out := syncOnce(t, b, "a")
	envOK(t, code == 0 && a.lines(t, u) == "from b, edited" && strings.Contains(out, "push a: 2 applied"), "push: code %d, out %q", code, out)
	w, _, _ := a.st.Watermark(b.id)
	envOK(t, w == 2, "peer watermark for us = %d, want 2", w)
	code, out = syncOnce(t, b, "a")
	envOK(t, code == 0 && strings.Contains(out, "push a: 0 applied"), "repeat push moved rows: %q", out)
}

func TestSyncPeersRefusesTruncatedStream(t *testing.T) {
	for name, cut := range map[string]func([]byte) []byte{
		"no end line":  func(b []byte) []byte { return b[:bytes.LastIndex(bytes.TrimRight(b, "\n"), []byte("\n"))+1] },
		"cut mid-line": func(b []byte) []byte { return b[:len(b)-12] },
		"wrong count":  func(b []byte) []byte { return bytes.Replace(b, []byte(`"count":2`), []byte(`"count":3`), 1) },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := newEnvPeer(t), newEnvPeer(t)
			defer pipeSSH(t, map[string]envPeer{"a": a}, cut)()
			u := a.localAdd(t, "notes/a.md", 0, "one")
			a.localEdit(t, u, "two")
			code, out := syncOnce(t, b, "a")
			w, _, _ := b.st.Watermark(a.id)
			envOK(t, code == 1 && strings.Contains(out, "pull a: refused") && w == 0 && len(docsOf(t, b)) == 0,
				"truncated stream: code %d, watermark %d, docs %q, out %q", code, w, docsOf(t, b), out)
		})
	}
}

func TestSyncPeersRefusesHeldSchema(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	a.localAdd(t, "notes/a.md", 0, "one")
	cut := func(raw []byte) []byte { return bytes.Replace(raw, []byte(`"schema":2`), []byte(`"schema":4`), 1) }
	defer pipeSSH(t, map[string]envPeer{"a": a}, cut)()
	code, out := syncOnce(t, b, "a")
	w, _, _ := b.st.Watermark(a.id)
	envOK(t, code == 1 && strings.Contains(out, "pull a: held") && w == 0 && len(docsOf(t, b)) == 0,
		"schema hold: code %d, watermark %d, docs %q, out %q", code, w, docsOf(t, b), out)
}

func TestSyncPeersPushesAfterHeldPull(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	a.localAdd(t, "notes/a.md", 0, "unsupported")
	u := b.localAdd(t, "notes/b.md", 0, "send despite hold")
	cut := func(raw []byte) []byte { return bytes.Replace(raw, []byte(`"schema":2`), []byte(`"schema":4`), 1) }
	defer pipeSSH(t, map[string]envPeer{"a": a}, cut)()
	code, out := syncOnce(t, b, "a")
	envOK(t, code == 1 && strings.Contains(out, "pull a: held") && a.lines(t, u) == "send despite hold",
		"held pull blocked push: code %d, out %q", code, out)
}

func TestSyncPeersRefusesNonprogressingPull(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	a.localAdd(t, "notes/a.md", 0, "one")
	_, err := pull(a, b)
	envOK(t, err == nil, "seed: %v", err)
	defer memory.StubSyncSSH(func(_ context.Context, _, _ string, _ io.Reader, out io.Writer) (string, int, error) {
		return "", 0, memory.ServeExport(a.st, nil, out)
	})()
	code, out := syncOnce(t, b, "a")
	envOK(t, code == 1 && strings.Contains(out, "pending"), "nonprogress: code %d, out %q", code, out)
}

func TestSyncPeersContinuesAfterRoundLimit(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	u := a.localAdd(t, "notes/a.md", 0, "0")
	for i := 1; i <= 20; i++ {
		a.localEdit(t, u, strconv.Itoa(i))
	}
	firstOnly := func(raw []byte) []byte {
		line := raw[:bytes.IndexByte(raw, '\n')+1]
		if bytes.Contains(line, []byte(`"end":true`)) {
			return raw
		}
		return append(bytes.Clone(line), []byte(`{"end":true,"count":1}`+"\n")...)
	}
	defer pipeSSH(t, map[string]envPeer{"a": a}, firstOnly)()
	code, out := syncOnce(t, b, "a")
	w, _, _ := b.st.Watermark(a.id)
	envOK(t, code == 1 && w == 20 && strings.Contains(out, "pending"), "first pass: exit %d, seq %d, out %q", code, w, out)
	code, out = syncOnce(t, b, "a")
	w, _, _ = b.st.Watermark(a.id)
	envOK(t, code == 0 && w == 21 && b.lines(t, u) == "20", "retry: exit %d, seq %d, out %q", code, w, out)
}

func TestImportStreamRollsBackOnSecondApplyError(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	u := a.localAdd(t, "notes/a.md", 0, "one")
	a.localEdit(t, u, "two")
	var stream bytes.Buffer
	_, err := memory.ExportEnvelopes(a.st, a.id, 0, &stream)
	envOK(t, err == nil, "export: %v", err)
	lines := bytes.Split(bytes.TrimSpace(stream.Bytes()), []byte("\n"))
	envOK(t, len(lines) == 2, "export lines = %d", len(lines))
	lines[1] = bytes.Replace(lines[1], []byte(`"entry_ref":"`+u+`"`), []byte(`"entry_ref":"bad"`), 1)
	_, err = memory.ImportStream(b.st, bytes.NewReader(bytes.Join(lines, []byte("\n"))))
	w, _, _ := b.st.Watermark(a.id)
	envOK(t, err != nil && w == 0 && len(docsOf(t, b)) == 0,
		"half import: watermark %d, docs %q (%v)", w, docsOf(t, b), err)
}

func TestSyncPeersRetriesBusyPeer(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	busy := true
	a.st.lock = func(f func() error) error {
		if busy {
			return memory.ErrBankBusy
		}
		return f()
	}
	defer pipeSSH(t, map[string]envPeer{"a": a}, nil)()
	u := b.localAdd(t, "notes/b.md", 0, "from b")
	code, out := syncOnce(t, b, "a")
	envOK(t, code == 1 && strings.Contains(out, "push a: busy") && len(docsOf(t, a)) == 0, "busy pass: code %d, out %q", code, out)
	busy = false
	code, out = syncOnce(t, b, "a")
	envOK(t, code == 0 && a.lines(t, u) == "from b", "retry pass: code %d, out %q", code, out)
}

func TestSyncPeersRelaysThirdOrigin(t *testing.T) {
	a, b, c := newEnvPeer(t), newEnvPeer(t), newEnvPeer(t)
	defer pipeSSH(t, map[string]envPeer{"a": a, "b": b}, nil)()
	u := a.localAdd(t, "notes/a.md", 0, "from a")
	c.localAdd(t, "notes/c.md", 0, "from c")
	for _, step := range [][2]envPeer{{b, a}, {c, b}, {b, a}} {
		code, out := syncOnce(t, step[0], map[string]string{a.id: "a", b.id: "b"}[step[1].id])
		envOK(t, code == 0, "step: code %d, out %q", code, out)
	}
	envOK(t, c.lines(t, u) == "from a" && len(docsOf(t, a)) == 2, "relay: c %q, a %q", c.lines(t, u), docsOf(t, a))
	sameDocs(t, a, b, c)
}

func TestImportStreamRollsBackOnCommitError(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	u := a.localAdd(t, "notes/a.md", 0, "one")
	for _, q := range []string{
		`CREATE TABLE commit_parent(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE commit_child(id INTEGER REFERENCES commit_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TRIGGER fail_commit AFTER INSERT ON peers BEGIN INSERT INTO commit_child VALUES(1); END`,
	} {
		_, err := b.db.Exec(q)
		envOK(t, err == nil, "commit failure fixture: %v", err)
	}
	var stream bytes.Buffer
	_, err := memory.ExportEnvelopes(a.st, a.id, 0, &stream)
	envOK(t, err == nil, "export: %v", err)
	rep, err := memory.ImportStream(b.st, bytes.NewReader(stream.Bytes()))
	w, _, werr := b.st.Watermark(a.id)
	envOK(t, err != nil && strings.Contains(err.Error(), "FOREIGN KEY") && rep.Applied == 0 && werr == nil && w == 0,
		"failed commit: report %+v, watermark %d (%v), error %v", rep, w, werr, err)
	envOK(t, len(docsOf(t, b)) == 0 && b.count(t, `SELECT count(*) FROM changes`) == 0, "failed commit left rows")
	_, err = b.db.Exec(`DROP TRIGGER fail_commit`)
	envOK(t, err == nil, "drop fixture: %v", err)
	rep, err = memory.ImportStream(b.st, bytes.NewReader(stream.Bytes()))
	envOK(t, err == nil && rep.Applied == 1 && b.lines(t, u) == "one", "retry: %+v (%v)", rep, err)
	rep, err = memory.ImportStream(b.st, bytes.NewReader(stream.Bytes()))
	envOK(t, err == nil && rep.Applied == 0 && rep.Dup == 1 && b.count(t, `SELECT count(*) FROM changes`) == 1,
		"replay: %+v (%v)", rep, err)
}

func TestSyncPeersHeldOriginDoesNotStarveLaterOrigins(t *testing.T) {
	for _, direction := range []string{"pull", "push"} {
		t.Run(direction, func(t *testing.T) {
			a, b, c := newEnvPeer(t), newEnvPeer(t), newEnvPeer(t)
			if a.id > b.id {
				a, b = b, a
			}
			a.localAdd(t, "notes/held.md", 0, strings.Repeat("x", 9<<20))
			u := b.localAdd(t, "notes/valid.md", 0, "valid later origin")
			send(t, b, a, b.id, 0)
			_, err := a.db.Exec(`UPDATE changes SET schema = 4 WHERE origin = ?`, a.id)
			envOK(t, err == nil, "schema fixture: %v", err)
			local, peer := c, a
			if direction == "push" {
				local, peer = a, c
			}
			defer pipeSSH(t, map[string]envPeer{"peer": peer}, nil)()
			code, out := syncOnce(t, local, "peer")
			w, _, err := c.st.Watermark(a.id)
			envOK(t, code == 1 && strings.Contains(out, "held") && err == nil && w == 0,
				"held origin: exit %d, watermark %d (%v), out %q", code, w, err, out)
			envOK(t, c.lines(t, u) == "valid later origin", "later origin starved: %q", out)
		})
	}
}

func TestSyncPeersOrderSchemaThreeAppliedByNewAndHeldByOld(t *testing.T) {
	for _, direction := range []string{"pull", "push"} {
		t.Run(direction, func(t *testing.T) {
			a, b, u := sharedOrder(t)
			old := newEnvPeer(t)
			meshSync(t, a, old)
			old.st.schema = memory.EnvelopeSchema
			a.writeBank(t, orderDoc, u[2], u[1], u[0])
			want := []string{u[2], u[1], u[0]}
			local, peer := b, a
			if direction == "push" {
				local, peer = a, b
			}
			defer pipeSSH(t, map[string]envPeer{"peer": peer, "old": old, "a": a}, nil)()
			code, out := syncOnce(t, local, "peer")
			envOK(t, code == 0 && slices.Equal(b.order(t, orderDoc), want), "new peer %s: code %d, order %v, out %q", direction, code, b.order(t, orderDoc), out)
			before, _, _ := old.st.Watermark(a.id)
			if direction == "pull" {
				code, out = syncOnce(t, old, "a")
			} else {
				code, out = syncOnce(t, a, "old")
			}
			after, _, _ := old.st.Watermark(a.id)
			envOK(t, code == 1 && strings.Contains(out, "held") && after == before && slices.Equal(old.order(t, orderDoc), u),
				"old peer %s: code %d, watermark %d->%d, order %v, out %q", direction, code, before, after, old.order(t, orderDoc), out)
		})
	}
}
