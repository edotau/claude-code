package redact

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/textutil"
)

const CountWindow = 1 << 20

// countPiece is the least text masked at once: a shape regex then scans only pieces holding its trigger word.
const countPiece = 8 << 10

// Counter counts what Mask would mask in a file, running Mask over pieces cut at line breaks no match crosses.
type Counter struct {
	matcher       *Matcher
	window, piece int
	buffers       sync.Pool
	Tally         *Tally // nil counts only; set before the first File
}

type countBuffers struct {
	data, scratch []byte
	log           []hit
}

// Tally breaks counted matches down by kind and vault needle, plus masks already present by byte length.
type Tally struct {
	mu      sync.Mutex
	Kinds   map[string]int
	Needles map[Origin]int
	Masks   map[int]int
}

// hit is one counted match (kind "vault" with its origin, or a shape kind) or an existing mask (kind "mask").
type hit struct {
	kind   string
	origin Origin
	mask   int
}

func NewTally() *Tally {
	return &Tally{Kinds: map[string]int{}, Needles: map[Origin]int{}, Masks: map[int]int{}}
}

func (t *Tally) add(log []hit) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, h := range log {
		switch h.kind {
		case "mask":
			t.Masks[h.mask]++
		case "vault":
			t.Kinds[h.kind]++
			t.Needles[h.origin]++
		default:
			t.Kinds[h.kind]++
		}
	}
}

// note logs the step at span: a value hit by the needle that starts it, or an existing mask (the only multi-byte miss).
func (m *Matcher) note(log *[]hit, span []byte, matched bool) {
	switch {
	case matched:
		n, _ := m.matchAt(span, true)
		*log = append(*log, hit{kind: "vault", origin: m.origins[string(span[:n])]})
	case len(span) > 1:
		*log = append(*log, hit{kind: "mask", mask: len(span)})
	}
}

// shapeFamilies are the credential prefixes SecretSpans recognises, longest first so github_pat_ beats gh.
var shapeFamilies = textutil.CredentialFamilies()

// shapeKind names the shape that produced span in text: pem, jwt, bearer, or prefix:<family>.
func shapeKind(text string, span [2]int) string {
	s := text[span[0]:span[1]]
	switch {
	case strings.HasPrefix(s, "-----BEGIN"):
		return "pem"
	case strings.HasPrefix(s, "eyJ"):
		return "jwt"
	case strings.HasSuffix(strings.ToLower(strings.TrimRight(text[:span[0]], "\t\n\f\r ")), "bearer"):
		return "bearer"
	}
	for _, p := range shapeFamilies {
		if strings.HasPrefix(s, p) {
			return "prefix:" + p
		}
	}
	return "shape:other"
}

// logFor is the hit log a count records into, or nil when no Tally wants one.
func (c *Counter) logFor(bufs *countBuffers) *[]hit {
	if c.Tally == nil {
		return nil
	}
	bufs.log = bufs.log[:0]
	return &bufs.log
}

func NewCounter(m *Matcher) *Counter {
	return &Counter{matcher: m, window: CountWindow, piece: countPiece}
}

func (c *Counter) Matcher() *Matcher { return c.matcher }

func (c *Counter) File(path string) (hits int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, errors.New("redact: count open failed")
	}
	defer func() {
		if f.Close() != nil {
			err = errors.New("redact: count close failed")
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return 0, errors.New("redact: count stat failed")
	}
	bufs, _ := c.buffers.Get().(*countBuffers)
	if bufs == nil {
		bufs = &countBuffers{}
	}
	hits, err = c.count(f, bufs)
	if cap(bufs.data) <= 4*c.window { // a buffer grown for one huge unbroken stretch is not kept
		c.buffers.Put(bufs)
	}
	if err != nil {
		return 0, err
	}
	current, err := f.Stat()
	if err != nil || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		return 0, errors.New("redact: file changed during count")
	}
	return hits, nil
}

// count reads r a window at a time and counts every cut piece; the rest is masked whole at end of input.
func (c *Counter) count(r io.Reader, bufs *countBuffers) (int, error) {
	hits, from := 0, 0
	buf := bufs.data[:0]
	defer func() { bufs.data = buf[:0] }()
	for {
		var eof bool
		var err error
		buf, eof, err = readUpTo(r, buf, len(buf)+c.window)
		if err != nil {
			return 0, err
		}
		n, cut, stop := c.segment(buf, from, bufs)
		hits += n
		if eof {
			log := c.logFor(bufs)
			n, _ := c.matcher.mask(buf[cut:], log)
			c.commit(log)
			return hits + n, nil
		}
		buf = buf[:copy(buf, buf[cut:])]
		from = stop - cut
	}
}

func readUpTo(r io.Reader, buf []byte, need int) ([]byte, bool, error) {
	buf = slices.Grow(buf, need-len(buf))
	for len(buf) < need {
		n, err := r.Read(buf[len(buf):need])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return buf, true, nil
		}
		if err != nil {
			return buf, false, errors.New("redact: count read failed")
		}
	}
	return buf, false, nil
}

// segment counts b in pieces ending at line breaks no match crosses, searching from from; stop is where it ended.
func (c *Counter) segment(b []byte, from int, bufs *countBuffers) (hits, cut, stop int) {
	end := max(from, c.piece)
	for end < len(b) {
		i := bytes.IndexByte(b[end:], '\n')
		if i < 0 {
			return hits, cut, len(b)
		}
		end += i + 1
		if c.matcher.crosses(b, end) {
			continue
		}
		bufs.scratch = append(bufs.scratch[:0], b[cut:end]...)
		log := c.logFor(bufs)
		n, open := c.matcher.mask(bufs.scratch, log)
		if open {
			end = cut + 2*(end-cut) // doubling keeps a long open PEM block from being masked once per line
			continue
		}
		c.commit(log)
		hits, cut, end = hits+n, end, end+c.piece
	}
	return hits, cut, end
}

func (c *Counter) commit(log *[]hit) {
	if log != nil {
		c.Tally.add(*log)
	}
}

// crosses reports a value occurrence spanning offset p of b, or one the bytes after p cannot yet settle.
func (m *Matcher) crosses(b []byte, p int) bool {
	if !m.breaks && len(b)-p >= m.longest { // b[p-1] is a line break, which no value then holds
		return false
	}
	for i := max(0, p-m.longest+1); i < p; i++ {
		if n, ok := m.matchAt(b[i:], false); !ok || i+n > p {
			return true
		}
	}
	return false
}
