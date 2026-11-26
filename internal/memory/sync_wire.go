package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	streamMax     = 8 << 20 // a serve export stops at an envelope boundary past this many bytes
	streamReadMax = 2 * streamMax
	sinceAll      = math.MaxInt64 // request-only skip for our own or a held origin; never persisted as an ack
)

var originRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// OriginLister is the optional store side of sync: its own origin and every origin it holds changes or a watermark for.
type OriginLister interface {
	Origins() (self string, all []string, err error)
}

// Batcher is the optional store side of one-transaction import: f's applies commit together or not at all.
type Batcher interface {
	Batch(f func(EnvelopeStore) error) error
}

type streamEnd struct {
	End   bool `json:"end"`
	Count int  `json:"count"`
}

// ParseSince reads "<origin>=<seq>,..." into watermarks; an origin or seq that is not plain text is refused.
func ParseSince(s string) (map[string]int64, error) {
	out := map[string]int64{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for pair := range strings.SplitSeq(s, ",") {
		o, n, ok := strings.Cut(pair, "=")
		seq, err := strconv.ParseInt(n, 10, 64)
		if !ok || !originRE.MatchString(o) || err != nil || seq < 0 {
			return nil, fmt.Errorf("memory sync: --since %q: want <origin>=<seq>,... with a plain origin and a nonnegative seq", pair)
		}
		if _, exists := out[o]; exists {
			return nil, fmt.Errorf("memory sync: --since repeats origin %q", o)
		}
		out[o] = seq
	}
	return out, nil
}

// FormatSince is ParseSince's inverse, sorted; origins that are not plain text are left out.
func FormatSince(m map[string]int64) string {
	var parts []string
	for o, n := range m {
		if originRE.MatchString(o) {
			parts = append(parts, o+"="+strconv.FormatInt(n, 10))
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// Watermarks is what st already holds per origin; its own origin counts as held in full.
func Watermarks(st EnvelopeStore) (map[string]int64, error) {
	l, ok := st.(OriginLister)
	if !ok {
		return nil, errors.New("memory sync: this envelope store cannot list its origins")
	}
	self, all, err := l.Origins()
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, o := range all {
		if !originRE.MatchString(o) {
			return nil, fmt.Errorf("memory sync: invalid stored origin %q", o)
		}
		if out[o], _, err = st.Watermark(o); err != nil {
			return nil, err
		}
	}
	if self != "" {
		out[self] = sinceAll
	}
	return out, nil
}

// ServeExport writes the changes past since as JSON lines ordered by origin then seq, capped, then the end line.
func ServeExport(st EnvelopeStore, since map[string]int64, w io.Writer) error {
	return serveExport(st, since, w, streamMax)
}

func serveExport(st EnvelopeStore, since map[string]int64, w io.Writer, limit int) error {
	l, ok := st.(OriginLister)
	if !ok {
		return errors.New("memory sync: this envelope store cannot list its origins")
	}
	_, all, err := l.Origins()
	if err != nil {
		return err
	}
	slices.Sort(all)
	x := &exporter{st: st, w: w, limit: limit}
	for _, o := range all {
		if !originRE.MatchString(o) {
			return fmt.Errorf("memory sync: invalid stored origin %q", o)
		}
		if capped, err := x.origin(o, since[o]); err != nil {
			return err
		} else if capped {
			break
		}
	}
	return json.NewEncoder(w).Encode(streamEnd{End: true, Count: x.n}) // a capped stream is a valid prefix; the next pass takes the rest
}

// exporter counts the rows and bytes written across origins.
type exporter struct {
	st             EnvelopeStore
	w              io.Writer
	n, size, limit int
}

// origin writes one origin's rows past after; capped means the size limit stopped the whole export.
func (x *exporter) origin(origin string, after int64) (capped bool, err error) {
	for {
		rows, err := x.st.ChangesAfter(origin, after, envelopePageSize)
		if err != nil {
			return false, fmt.Errorf("memory sync: export %q: %w", origin, err)
		}
		for _, e := range rows {
			if e.Origin != origin || e.Seq != after+1 {
				return false, fmt.Errorf("memory sync: export %q: seq %d after %d is out of order", origin, e.Seq, after)
			}
			line, err := json.Marshal(e)
			if err != nil {
				return false, err
			}
			if len(line)+64 > streamReadMax {
				return false, fmt.Errorf("memory sync: envelope %q/%d exceeds the stream size cap", origin, e.Seq)
			}
			if x.size += len(line) + 1; x.size > x.limit && x.n > 0 {
				return true, nil
			}
			if _, err := x.w.Write(append(line, '\n')); err != nil {
				return false, err
			}
			x.n, after = x.n+1, e.Seq
		}
		if len(rows) < envelopePageSize {
			return false, nil
		}
	}
}

// ReadStream returns the envelope lines of a stream ending in {"end":true,"count":N} with N right, else refuses it.
func ReadStream(r io.Reader) (body []byte, count int, err error) {
	raw, err := io.ReadAll(io.LimitReader(r, streamReadMax+1))
	if err != nil {
		return nil, 0, fmt.Errorf("memory sync: read stream: %w", err)
	}
	if len(raw) > streamReadMax {
		return nil, 0, errors.New("memory sync: stream is over the size cap; refused whole")
	}
	trimmed := bytes.TrimRight(raw, "\r\n ")
	i := bytes.LastIndexByte(trimmed, '\n')
	end := streamEnd{Count: -1}
	dec := json.NewDecoder(bytes.NewReader(trimmed[i+1:]))
	dec.DisallowUnknownFields()
	if len(trimmed) == 0 || dec.Decode(&end) != nil || !end.End {
		return nil, 0, errors.New("memory sync: stream has no end line (cut off); refused whole")
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, 0, errors.New("memory sync: stream has data after its end line; refused whole")
	}
	body = trimmed[:i+1]
	n := 0
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return nil, 0, errors.New("memory sync: stream has an invalid envelope line; refused whole")
		}
		n++
	}
	if n != end.Count || n != bytes.Count(body, []byte("\n")) {
		return nil, 0, fmt.Errorf("memory sync: stream holds %d envelopes but its end line says %d; refused whole", n, end.Count)
	}
	return body, end.Count, nil
}

// ImportStream applies r in one transaction when the store offers it; an error leaves nothing applied.
func ImportStream(st EnvelopeStore, r io.Reader) (rep ImportReport, err error) {
	b, ok := st.(Batcher)
	if !ok {
		return rep, errors.New("memory sync: envelope store cannot import atomically")
	}
	schema := EnvelopeMaxSchema
	if s, ok := st.(interface{ LocalSchema() int }); ok && s.LocalSchema() > 0 {
		schema = s.LocalSchema()
	}
	err = b.Batch(func(tx EnvelopeStore) (e error) { rep, e = ImportEnvelopes(tx, r, schema); return })
	if err != nil {
		rep = ImportReport{Errors: rep.Errors}
	}
	return rep, err
}

type serveSince struct {
	Since map[string]int64 `json:"since"`
}

type serveApplied struct {
	Applied     int      `json:"applied"`
	Dup         int      `json:"dup"`
	Conflicts   int      `json:"conflicts"`
	Held        []string `json:"held,omitempty"`
	HeldOrigins []string `json:"held_origins,omitempty"`
}

// ServeImport is the peer side of a push: print our watermarks, read the stream from in, import it, print the counts.
func ServeImport(st EnvelopeStore, in io.Reader, out io.Writer) error {
	since, err := Watermarks(st)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	if err := enc.Encode(serveSince{Since: since}); err != nil {
		return err
	}
	body, _, err := ReadStream(in)
	if err != nil {
		return err
	}
	rep, err := ImportStream(st, bytes.NewReader(body))
	if err != nil {
		return err
	}
	return enc.Encode(serveApplied{Applied: rep.Applied, Dup: rep.Dup, Conflicts: rep.Conflicts, Held: rep.Held, HeldOrigins: rep.HeldOrigins})
}
