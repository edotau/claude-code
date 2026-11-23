// interact.go is the `agents dashboard` terminal loop: raw keys, periodic reload, Enter opens a job's live output.

package tree

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
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
)

// jobFooter is the key hint under the detail view.
const jobFooter = "esc/q back"

// pollEvery is how often the dashboard checks the terminal size; minInterval is the shortest listing reload.
const (
	pollEvery   = 250 * time.Millisecond
	minInterval = 500 * time.Millisecond
)

// keySeqs are the byte sequences that name a key: arrows in CSI and SS3 form, vi hjkl, Enter and q.
var keySeqs = []struct {
	seq string
	k   key
}{{"\x1b[A", keyUp}, {"\x1bOA", keyUp}, {"\x1b[B", keyDown}, {"\x1bOB", keyDown},
	{"\x1b[C", keyRight}, {"\x1bOC", keyRight}, {"\x1b[D", keyLeft}, {"\x1bOD", keyLeft},
	{"k", keyUp}, {"j", keyDown}, {"l", keyRight}, {"h", keyLeft}, {"\r", keyEnter}, {"\n", keyEnter}, {"q", keyQuit}}

// decodeKeys maps raw terminal bytes to keys: keySeqs, then a lone Esc; any other sequence or byte drops.
func decodeKeys(b []byte) []key {
	var out []key
	for len(b) > 0 {
		n, k, ok := decodeKey(b)
		if ok {
			out = append(out, k)
		}
		b = b[n:]
	}
	return out
}

// decodeKey reads one key off the front of b: the bytes it used, and the key when they name one.
func decodeKey(b []byte) (n int, k key, ok bool) {
	for _, s := range keySeqs {
		if strings.HasPrefix(string(b), s.seq) {
			return len(s.seq), s.k, true
		}
	}
	switch {
	case b[0] == 0x1b && len(b) >= 3 && (b[1] == '[' || b[1] == 'O'):
		return 3, 0, false
	case b[0] == 0x1b:
		return 1, keyEsc, true
	}
	return 1, 0, false
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
	d := &dashboard{ctx: ctx, claudeDir: claudeDir, opts: opts, view: v, keys: keys,
		f: frame{paint: activePaint(os.Stdout)}, st: dashState{width: w, height: h}}
	return d.run()
}

// run reloads the listing every interval, polls the terminal size and redraws until a quit key or signal.
func (d *dashboard) run() error {
	poll := time.NewTicker(pollEvery) // no SIGWINCH on Windows builds, so a resize is found by polling
	defer poll.Stop()
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
		case <-poll.C:
		case k, ok := <-d.keys:
			if !ok || k == keyQuit || k == keyEsc {
				return nil
			}
			d.onKey(k)
		}
	}
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

// onKey opens the selected dispatch's detail on Enter, else applies the key to the view state.
func (d *dashboard) onKey(k key) {
	if k != keyEnter {
		d.st = onKey(d.st, k, d.nodes)
		return
	}
	vis := visible(d.nodes, d.st.open)
	if d.st.sel < len(vis) && vis[d.st.sel].Kind == KindDispatch {
		n := vis[d.st.sel]
		d.openJob(cmp.Or(n.Via, n.ID)) // a folded row opens its carrier, as `agents tree` lists it
	}
}

// draw lays the frame out for the current terminal size and writes it without a trailing newline, so nothing scrolls.
func (d *dashboard) draw() error {
	if w, h := termSize(); w != d.st.width || h != d.st.height {
		d.st = onResize(d.st, w, h)
	}
	d.f.text.Reset()
	d.f.text.WriteString(strings.Join(dashLines(d.st, d.nodes, time.Now(), d.f.paint), "\n"))
	return d.f.flush(os.Stdout)
}

// openJob shows one job's detail and follows its output, as `agents result -f <id>` does, until Esc or q.
func (d *dashboard) openJob(id string) {
	rec, err := agent.LoadRecord(id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(d.ctx)
	out, done := follow(ctx, rec)
	defer func() { cancel(); <-done }()
	head := jobHeader(d.tree, id, rec)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if d.drawJob(head, out.tail()) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case k, ok := <-d.keys:
			if !ok || k == keyEsc || k == keyQuit {
				return
			}
		}
	}
}

// drawJob draws the detail view of head and output, sized to the terminal.
func (d *dashboard) drawJob(head, output []string) error {
	width, height := termSize()
	d.f.text.Reset()
	for _, line := range detailLines(head, output, width, height) {
		fmt.Fprintln(&d.f.text, line)
	}
	return d.f.flush(os.Stdout)
}

// follow copies rec's log into a buffer until ctx ends or the job does; done closes when the copy stops.
func follow(ctx context.Context, rec agent.Record) (*followBuf, <-chan struct{}) {
	out, done := &followBuf{}, make(chan struct{})
	go func() {
		defer close(done)
		cwd, _ := os.Getwd()
		if _, err := agent.Tail(ctx, rec.Agent, cwd, rec.ID, true, out); err != nil && ctx.Err() == nil {
			fmt.Fprintf(out, "\n· %v\n", err)
		}
	}()
	return out, done
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
		"task   " + textutil.OneLine(briefTask(rec.Request.Prompt)),
	}
	if desc := descriptionOf(tr, id); desc != "" {
		head = append(head, "about  "+textutil.OneLine(desc))
	}
	return head
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

// detailLines fits head (the task wrapped to three lines), a rule and the newest output lines into width x height.
func detailLines(head, output []string, width, height int) []string {
	var lines []string
	for _, h := range head {
		limit := 1
		if strings.HasPrefix(h, "task ") {
			limit = 3
		}
		lines = append(lines, wrapCells(printable(h), width, limit)...)
	}
	lines = append(lines, strings.Repeat("─", min(width, 80)))
	room := max(height-len(lines)-2, 0)
	output = output[max(len(output)-room, 0):]
	for _, o := range output {
		lines = append(lines, textutil.TruncateCells(o, width, "…"))
	}
	return append(lines, "", jobFooter)
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

// followBuf keeps the newest tailKeep lines a follow writes, terminal escapes and control bytes stripped.
type followBuf struct {
	mu      sync.Mutex
	lines   []string
	partial string
}

const tailKeep = 500

func (t *followBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	parts := strings.Split(t.partial+string(p), "\n")
	t.partial = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		t.lines = append(t.lines, printable(line))
	}
	if n := len(t.lines); n > tailKeep {
		t.lines = slices.Clone(t.lines[n-tailKeep:])
	}
	return len(p), nil
}

// tail is the kept lines, then the partial line still being written.
func (t *followBuf) tail() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.partial == "" {
		return slices.Clone(t.lines)
	}
	return append(slices.Clone(t.lines), printable(t.partial))
}

// printable is line without escape sequences or control bytes, a tab as spaces and a carriage return's overwrite.
func printable(line string) string {
	line = ansi.Strip(line)
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
