// interact.go is the interactive --watch: arrow keys select a dispatch row, Enter opens it with its live output.

package tree

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
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

// key is one keypress the interactive watch reacts to.
type key int

const (
	keyUp key = iota
	keyDown
	keyEnter
	keyEsc
	keyQuit
)

// listFooter and jobFooter are the key hints under the list and the detail view.
const (
	listFooter = "↑/↓ select · enter open · q quit"
	jobFooter  = "esc/q back"
)

// keySeqs are the byte sequences that name a key: arrows in CSI and SS3 form, vi j/k, Enter and q.
var keySeqs = []struct {
	seq string
	k   key
}{{"\x1b[A", keyUp}, {"\x1bOA", keyUp}, {"\x1b[B", keyDown}, {"\x1bOB", keyDown},
	{"k", keyUp}, {"j", keyDown}, {"\r", keyEnter}, {"\n", keyEnter}, {"q", keyQuit}}

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

// rawKeys turns off echo and line buffering and streams keys; restore puts the terminal back and shows the cursor.
func rawKeys() (keys <-chan key, restore func(), err error) {
	saved, err := stty("-g")
	if err != nil {
		return nil, nil, err
	}
	if _, err := stty("-icanon", "-echo", "min", "1"); err != nil {
		return nil, nil, err
	}
	fmt.Print("\x1b[?25l")
	ch := make(chan key, 8)
	go func() { // parks in read(2) until a key or exit; the verb ends with the watch, so it never steals input
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
	return ch, func() { fmt.Print("\x1b[?25h"); _, _ = stty(saved) }, nil
}

// watcher is one interactive watch: the newest listing, the selected dispatch id and the key stream.
type watcher struct {
	ctx       context.Context
	claudeDir string
	opts      Options
	view      View
	keys      <-chan key
	f         frame
	tree      *Tree
	sel       string
}

// watchKeys runs the interactive watch when stdin and stdout are terminals; ok false leaves the plain watch to run.
func watchKeys(claudeDir string, opts Options, v View) (code int, ok bool) {
	if v.JSON || runtime.GOOS == "windows" || !proc.IsTerminal(os.Stdin) || rowWidth(os.Stdout) == 0 {
		return 0, false
	}
	keys, restore, err := rawKeys()
	if err != nil {
		return 0, false
	}
	defer restore()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	w := &watcher{ctx: ctx, claudeDir: claudeDir, opts: opts, view: v, keys: keys, f: frame{paint: activePaint(os.Stdout)}}
	return w.run(), true
}

// run reloads the listing every interval and redraws it on each key until q, Ctrl-C or a failed load.
func (w *watcher) run() int {
	tick := time.NewTicker(w.view.Interval)
	defer tick.Stop()
	for {
		tr, err := listing(w.claudeDir, w.opts, w.view.All)
		if err != nil {
			fmt.Fprintln(os.Stderr, "agents tree:", err)
			return 1
		}
		tr.detail, w.tree = w.view.All, tr
		for reload := false; !reload; {
			ids := jobIDs(tr.Dispatches)
			w.sel = pick(ids, w.sel, 0)
			if err := w.drawList(); err != nil {
				fmt.Fprintln(os.Stderr, "agents tree:", err)
				return 1
			}
			select {
			case <-w.ctx.Done():
				return 0
			case <-tick.C:
				reload = true
			case k, ok := <-w.keys:
				if !ok || k == keyQuit {
					return 0
				}
				reload = w.onKey(k, ids)
			}
		}
	}
}

// onKey moves the selection, or opens the selected job; true when the listing should reload.
func (w *watcher) onKey(k key, ids []string) bool {
	switch k {
	case keyUp:
		w.sel = pick(ids, w.sel, -1)
	case keyDown:
		w.sel = pick(ids, w.sel, 1)
	case keyEnter:
		if w.sel != "" {
			w.openJob(w.sel)
			return true
		}
	}
	return false
}

// jobIDs are the dispatch rows' ids in the order the list prints them.
func jobIDs(ds []Dispatch) []string {
	var ids []string
	for _, n := range jobRows(ds) {
		ids = append(ids, n.ID)
	}
	return ids
}

// pick moves the selection by step over ids, clamped; a vanished or empty selection lands on the first row.
func pick(ids []string, sel string, step int) string {
	if len(ids) == 0 {
		return ""
	}
	i := slices.Index(ids, sel)
	if i < 0 {
		return ids[0]
	}
	return ids[min(max(i+step, 0), len(ids)-1)]
}

// drawList draws the listing with the selection reversed and the key hints below it.
func (w *watcher) drawList() error {
	w.tree.selected = w.sel
	w.f.width = rowWidth(os.Stdout)
	w.f.text.Reset()
	renderTree(&w.f.text, w.tree, time.Now(), w.f.width, w.f.paint)
	fmt.Fprintf(&w.f.text, "\n%s\n", ansi.Style(w.f.paint != "", ansi.Dim, listFooter))
	return w.f.flush(os.Stdout)
}

// openJob shows one job's detail and follows its output, as `agents result -f <id>` does, until Esc or q.
func (w *watcher) openJob(id string) {
	rec, err := agent.LoadRecord(id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(w.ctx)
	out, done := follow(ctx, rec)
	defer func() { cancel(); <-done }()
	head := jobHeader(w.tree, id, rec)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if w.drawJob(head, out.tail()) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case k, ok := <-w.keys:
			if !ok || k == keyEsc || k == keyQuit {
				return
			}
		}
	}
}

// drawJob draws the detail view of head and output, sized to the terminal.
func (w *watcher) drawJob(head, output []string) error {
	width, height := termSize()
	w.f.text.Reset()
	for _, line := range detailLines(head, output, width, height) {
		fmt.Fprintln(&w.f.text, line)
	}
	return w.f.flush(os.Stdout)
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
