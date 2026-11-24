// interact.go is the `agents dashboard` terminal loop: raw keys, periodic reload, Enter opens a job's live output.

package tree

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// key is one keypress the dashboard reacts to.
type key int

const (
	keyUp key = iota
	keyDown
	keyLeft
	keyRight
	keyEnter
	keyEsc
	keyQuit
	keyCancel
	keyCopy
	keyResult
	keyConfirm
	keyOther
)

// jobFooter is the key hint under the detail view.
const jobFooter = "esc/q back"

// pollEvery is how often the dashboard checks the terminal size; minInterval is the shortest listing reload.
const (
	pollEvery   = 250 * time.Millisecond
	minInterval = 500 * time.Millisecond
)

// keySeqs are the byte sequences that name a key: arrows in CSI and SS3 form, vi hjkl, Enter, q and the actions.
var keySeqs = []struct {
	seq string
	k   key
}{{"\x1b[A", keyUp}, {"\x1bOA", keyUp}, {"\x1b[B", keyDown}, {"\x1bOB", keyDown},
	{"\x1b[C", keyRight}, {"\x1bOC", keyRight}, {"\x1b[D", keyLeft}, {"\x1bOD", keyLeft},
	{"k", keyUp}, {"j", keyDown}, {"l", keyRight}, {"h", keyLeft}, {"\r", keyEnter}, {"\n", keyEnter}, {"q", keyQuit},
	{"c", keyCancel}, {"y", keyCopy}, {"r", keyResult}, {"Y", keyConfirm}}

// decodeKeys maps raw terminal bytes to keys: keySeqs, then a lone Esc; any other sequence or byte is keyOther.
func decodeKeys(b []byte) []key {
	var out []key
	for len(b) > 0 {
		n, k := decodeKey(b)
		out = append(out, k)
		b = b[n:]
	}
	return out
}

// decodeKey reads one key off the front of b: the bytes it used and the key; unknown keys still disarm a cancel.
func decodeKey(b []byte) (n int, k key) {
	for _, s := range keySeqs {
		if strings.HasPrefix(string(b), s.seq) {
			return len(s.seq), s.k
		}
	}
	switch {
	case b[0] == 0x1b && len(b) >= 3 && (b[1] == '[' || b[1] == 'O'):
		return 3, keyOther
	case b[0] == 0x1b:
		return 1, keyEsc
	}
	return 1, keyOther
}

// stty runs stty on the terminal: stdin is the device it reads and sets.
func stty(args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// rawKeys enters the alternate screen without echo or line buffering and streams keys; restore undoes all of it.
func rawKeys() (keys <-chan key, restore func(), err error) {
	saved, err := stty("-g")
	if err != nil {
		return nil, nil, err
	}
	if _, err := stty("-icanon", "-echo", "min", "1"); err != nil {
		return nil, nil, err
	}
	fmt.Print("\x1b[?1049h\x1b[?25l")
	ch := make(chan key, 8)
	go func() { // parks in read(2) until a key or exit; the verb ends with the dashboard, so it never steals input
		defer close(ch)
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			for _, k := range decodeKeys(buf[:n]) {
				ch <- k
			}
			if err != nil {
				return
			}
		}
	}()
	return ch, func() { fmt.Print("\x1b[?25h\x1b[?1049l"); _, _ = stty(saved) }, nil
}

// dashboard is one `agents dashboard` run: the newest listing as nodes, the view state and the key stream.
type dashboard struct {
	ctx       context.Context
	claudeDir string
	opts      Options
	view      View
	keys      <-chan key
	out       io.Writer
	f         frame
	tree      *Tree
	nodes     []Node
	st        dashState
	loaded    time.Time
}

// Dashboard runs the walkable tree until q, Esc, a signal or a failed load; stdin and stdout must be terminals.
func Dashboard(claudeDir string, opts Options, v View) int {
	if err := runDashboard(claudeDir, opts, v); err != nil {
		fmt.Fprintln(os.Stderr, "agents dashboard:", err) // after the restore, so it lands on the main screen
		return 1
	}
	return 0
}

// runDashboard owns the terminal: its deferred restore runs on return, panic and SIGINT/SIGTERM/SIGHUP alike.
func runDashboard(claudeDir string, opts Options, v View) error {
	opts.scans = loadScanCache()
	keys, restore, err := rawKeys()
	if err != nil {
		return err
	}
	defer restore()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	w, h := termSize()
	d := &dashboard{ctx: ctx, claudeDir: claudeDir, opts: opts, view: v, keys: keys, out: os.Stdout,
		f: frame{paint: activePaint(os.Stdout)}, st: dashState{width: w, height: h}}
	return d.run()
}

// run reloads the listing every interval, polls the terminal size and redraws until a quit key or signal.
func (d *dashboard) run() error {
	poll := time.NewTicker(pollEvery) // no SIGWINCH on Windows builds, so a resize is found by polling
	defer poll.Stop()
	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()
	headers := make(chan headerStatus, 1)
	go refreshHeader(ctx, d.claudeDir, headers)
	for {
		if err := d.reloadDue(); err != nil {
			return err
		}
		if err := d.draw(); err != nil {
			return err
		}
		select {
		case <-d.ctx.Done():
			return nil
		case h := <-headers:
			d.st.header = &h
			d.st = clampTop(d.st)
		case <-poll.C:
		case k, ok := <-d.keys:
			if d.keyQuits(k, ok) {
				return nil
			}
		}
	}
}

// keyQuits reports a quit key or a closed stream, else applies the key.
func (d *dashboard) keyQuits(k key, ok bool) bool {
	if !ok || k == keyQuit || k == keyEsc {
		return true
	}
	d.onKey(k)
	return false
}

// reloadDue lists the tree again once an interval has passed, keeping the selection on the same node.
func (d *dashboard) reloadDue() error {
	if time.Since(d.loaded) < max(d.view.Interval, minInterval) {
		return nil
	}
	tr, err := listing(d.claudeDir, d.opts, d.view.All)
	if err != nil {
		return err
	}
	d.loaded, d.tree, d.nodes = time.Now(), tr, Build(tr.Sessions, tr.Dispatches)
	d.st = clamp(d.st, visible(d.nodes, d.st.open))
	return nil
}

// onKey opens the selected dispatch's pane on Enter with no cancel armed, else applies the key and performs its effect.
func (d *dashboard) onKey(k key) {
	vis := visible(d.nodes, d.st.open)
	if k == keyEnter && d.st.pending == "" && d.st.sel < len(vis) {
		switch n := vis[d.st.sel]; {
		case n.Kind == KindDispatch:
			d.showJob(actionTarget(n)) // a folded row opens its carrier, as `agents tree` lists it
			return
		case n.Kind == KindSubagent && n.Subagent != nil:
			d.showSubagent(*n.Subagent)
			return
		}
	}
	var e effect
	d.st, e = onKey(d.st, k, d.nodes)
	d.st.status = perform(e, d.out)
}

// draw lays the frame out for the current terminal size and writes it without a trailing newline, so nothing scrolls.
func (d *dashboard) draw() error {
	if w, h := termSize(); w != d.st.width || h != d.st.height {
		d.st = onResize(d.st, w, h)
	}
	return d.flush(dashLines(d.st, d.nodes, time.Now(), d.f.paint))
}

func (d *dashboard) flush(lines []string) error {
	d.f.text.Reset()
	d.f.text.WriteString(strings.Join(lines, "\n"))
	return d.f.flush(d.out)
}

// showJob follows one job's event stream in the detail pane, as `agents result -f --events <id>` does, until Esc or q.
func (d *dashboard) showJob(id string) {
	rec, err := agent.LoadRecord(id)
	if err != nil {
		d.st.status = actionLine(fmt.Sprintf("open %s: %v", id, err))
		return
	}
	d.runPane(rec, jobHeader(d.tree, id, rec), &paneFeeds{ev: openPane(d.ctx, rec), rawFn: paneLog}, newPaneSet())
}

// showSubagent follows one subagent's transcript in the detail pane, as showJob does a dispatch, until Esc or q.
func (d *dashboard) showSubagent(sub Subagent) {
	path := transcriptPath(d.claudeDir, sub.ID)
	if path == "" {
		d.st.status = actionLine("open " + sub.ID + ": no transcript found")
		return
	}
	s := newPaneSet()
	s.ev.noLog = true
	head := subagentHeader(sub, transcriptStart(path), time.Now())
	d.runPane(agent.Record{}, head, &paneFeeds{ev: openFeed(d.ctx, agent.Record{}, transcriptStream(path))}, s)
}

// runPane draws the pane's frames until Esc or q, closing feeds on the way out.
func (d *dashboard) runPane(rec agent.Record, head []string, feeds *paneFeeds, s paneSet) {
	defer feeds.close()
	var err error
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if s, err = d.drawPane(s, head, feeds); err != nil {
			return
		}
		select {
		case <-d.ctx.Done():
			return
		case <-tick.C:
		case k, ok := <-d.keys:
			if !ok || k == keyEsc || k == keyQuit {
				return
			}
			s = feeds.onKey(d.ctx, rec, s, k)
		}
	}
}

// drawPane takes the lines read since the last frame and draws the shown view at the terminal's size.
func (d *dashboard) drawPane(s paneSet, head []string, f *paneFeeds) (paneSet, error) {
	w, h := termSize()
	rows := paneRows(head, w, h)
	s.ev, s.raw = paneResize(s.ev, rows), paneResize(s.raw, rows)
	s = paneDrain(s, f)
	return s, d.flush(paneLines(s.cur(), head, w, h))
}

// termSize is stdout's columns and rows, 80x24 when it cannot be read.
func termSize() (width, height int) {
	width, height, err := proc.Size(int(os.Stdout.Fd()))
	if err != nil || width <= 0 || height <= 0 {
		return 80, 24
	}
	return width, height
}

// jobHeader is the detail view's top: agent, model and route, dispatch id, the brief's task, the description.
func jobHeader(tr *Tree, id string, rec agent.Record) []string {
	d := dispatchFromRecord(rec, "", time.Now())
	rows := jobRows(tr.Dispatches)
	if i := slices.IndexFunc(rows, func(n nested) bool { return n.ID == id }); i >= 0 {
		d = rows[i].Dispatch // the folded row: the carried child's model and route
	}
	head := []string{
		"agent  " + d.Agent,
		"model  " + strings.TrimPrefix(cmp.Or(d.Model, "default"), databricksPrefix) + " · " + d.where(),
		"id     " + id,
		"task   " + paneText(briefTask(rec.Request.Prompt)),
	}
	if desc := descriptionOf(tr, id); desc != "" {
		head = append(head, "about  "+paneText(desc))
	}
	return head
}

// subagentHeader is the subagent pane's top: type, model, description, time since start and tokens.
func subagentHeader(sub Subagent, start, now time.Time) []string {
	end := sub.Updated
	if sub.Status == statusRunning || end.IsZero() {
		end = now
	}
	head := []string{
		"agent  " + cmp.Or(sub.Type, "subagent"),
		"model  " + strings.TrimPrefix(subModel(sub), databricksPrefix),
		"about  " + paneText(sub.Description),
	}
	if !start.IsZero() {
		head = append(head, "time   "+shortDuration(end.Sub(start))+" · "+sub.Status)
	}
	return append(head, "tokens ↑ "+textutil.CompactCount(sub.Tokens.In)+" · ↓ "+textutil.CompactCount(sub.Tokens.Out))
}

// transcriptPath finds subagent id's agent-<id>.jsonl under a session or a workflow run; "" when absent or id unsafe.
func transcriptPath(claudeDir, id string) string {
	if !plainSessionID(id) {
		return ""
	}
	for _, pat := range []string{"*/*/subagents/agent-" + id + ".jsonl", "*/*/subagents/workflows/*/agent-" + id + ".jsonl"} {
		if m, _ := filepath.Glob(filepath.Join(claudeDir, "projects", pat)); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// transcriptStart is the first timestamp in the transcript's opening lines; zero when none shows.
func transcriptStart(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	sc := newLineScanner(f)
	for range 30 {
		if !sc.Scan() {
			break
		}
		var rec struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Timestamp != "" {
			return parseRFC3339(rec.Timestamp)
		}
	}
	return time.Time{}
}

// descriptionOf is the description of the subagent that launched dispatch id; "" when no subagent row did.
func descriptionOf(tr *Tree, id string) string {
	has := func(d Dispatch) bool { return d.ID == id }
	for _, s := range tr.Sessions {
		subs := slices.Clone(s.Subagents)
		for _, run := range s.Workflows {
			forEachRunAgent(run, func(sub Subagent) { subs = append(subs, sub) })
		}
		for _, sub := range subs {
			if slices.ContainsFunc(sub.Dispatches, has) {
				return sub.Description
			}
		}
	}
	return ""
}

// wrapCells breaks s at spaces into at most limit lines of width cells, the last clipped with an ellipsis.
func wrapCells(s string, width, limit int) []string {
	var out []string
	for s != "" && len(out) < limit-1 && textutil.DisplayWidth(s) > width {
		cut := textutil.TruncateCells(s, width, "")
		if i := strings.LastIndexByte(cut, ' '); i > 0 {
			cut = cut[:i]
		}
		out = append(out, cut)
		s = strings.TrimLeft(s[len(cut):], " ")
	}
	return append(out, textutil.TruncateCells(s, width, "…"))
}

// printable is line without escape sequences or control bytes, a tab as spaces and a carriage return's overwrite.
func printable(line string) string {
	line = stripEsc(line)
	if i := strings.LastIndexByte(strings.TrimRight(line, "\r"), '\r'); i >= 0 {
		line = line[i+1:]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, line)
}
