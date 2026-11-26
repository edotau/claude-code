package memory

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// Envelope ops and the payload schema: schema 2 addresses entries by uid (entries.uid).
const (
	EnvelopeSchema      = 2 // upsert and delete
	EnvelopeOrderSchema = 3 // order changes: an older reader holds the stream at the first one
	EnvelopeMaxSchema   = EnvelopeOrderSchema
	envelopePageSize    = 256
	EnvelopeOpUpsert    = "upsert"
	EnvelopeOpDelete    = "delete"
	EnvelopeOpOrder     = "order" // one document's published entry order; addressed by DocRef, not EntryRef
)

// OpSchema is the wire schema an op travels on.
func OpSchema(op string) int {
	if op == EnvelopeOpOrder {
		return EnvelopeOrderSchema
	}
	return EnvelopeSchema
}

// Envelope is one changes row on the wire; BaseVersion is "<origin>/<seq>" of the change it was made over.
type Envelope struct {
	Origin      string          `json:"origin"`
	Seq         int64           `json:"origin_seq"`
	Schema      int             `json:"schema"`
	EntryRef    string          `json:"entry_ref"`
	DocRef      string          `json:"doc_ref,omitempty"`
	BaseVersion string          `json:"base_version"`
	Op          string          `json:"op"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	At          string          `json:"at"`
}

// ConflictRow is one sync conflict: the peer envelope that was kept apart, not applied.
type ConflictRow struct {
	ID         int64           `json:"id"`
	Kind       string          `json:"kind"`
	Op         string          `json:"op"`
	Doc        string          `json:"doc"`
	EntryRef   string          `json:"entry_ref"`
	PeerOrigin string          `json:"peer_origin"`
	PeerSeq    int64           `json:"peer_seq"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	At         string          `json:"at"`
	Resolved   bool            `json:"resolved"`
}

// EnvelopeStore is the db side of sync; ApplyEnvelope runs apply, change row, conflict and ack in one transaction.
type EnvelopeStore interface {
	ChangesAfter(origin string, afterSeq int64, limit int) ([]Envelope, error) // a short page means exhausted
	Watermark(origin string) (acked int64, schema int, err error)
	ApplyEnvelope(e Envelope) (conflict bool, err error)
	Conflicts(openOnly bool) ([]ConflictRow, error)
	ResolveConflict(id int64, keep string) error
	Close() error
}

// OpenEnvelopeStore is set by internal/memory/shadow, keeping SQLite out of the hook binary.
var OpenEnvelopeStore func() (EnvelopeStore, error)

// ImportReport counts one import: applied includes conflicts; held names each origin stopped at a gap or schema.
type ImportReport struct {
	Applied, Dup, Conflicts   int
	Held, HeldOrigins, Errors []string
}

// ExportEnvelopes writes origin's changes after afterSeq as JSON lines in seq order; schemas pass through for relays.
func ExportEnvelopes(st EnvelopeStore, origin string, afterSeq int64, w io.Writer) (int, error) {
	if strings.TrimSpace(origin) == "" || afterSeq < 0 {
		return 0, errors.New("memory envelopes: origin is required and afterSeq must be nonnegative")
	}
	x := &exporter{st: st, w: w, tag: "memory envelopes"}
	_, err := x.origin(origin, afterSeq)
	return x.n, err
}

// ImportEnvelopes applies each origin in seq order; a gap or schema hold leaves its watermark for the sender's retry.
func ImportEnvelopes(st EnvelopeStore, r io.Reader, localSchema int) (ImportReport, error) {
	var rep ImportReport
	if localSchema <= 0 {
		return rep, errors.New("memory envelopes: local schema must be positive")
	}
	groups, err := readEnvelopes(r, localSchema)
	if err != nil {
		return rep, err
	}
	var failed []error
	for _, origin := range slices.Sorted(maps.Keys(groups)) {
		rows := groups[origin]
		slices.SortStableFunc(rows, func(a, b Envelope) int { return cmp.Compare(a.Seq, b.Seq) })
		if err := importOrigin(st, origin, rows, localSchema, &rep); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", origin, err))
			failed = append(failed, err)
			break
		}
	}
	return rep, errors.Join(failed...)
}

func readEnvelopes(r io.Reader, localSchema int) (map[string][]Envelope, error) {
	groups := map[string][]Envelope{}
	dec := json.NewDecoder(r)
	for {
		var e Envelope
		err := dec.Decode(&e)
		if errors.Is(err, io.EOF) {
			return groups, nil
		}
		if err != nil {
			return nil, fmt.Errorf("memory envelopes: decode: %w", err)
		}
		if err := checkEnvelope(e, localSchema); err != nil {
			return nil, err
		}
		groups[e.Origin] = append(groups[e.Origin], e)
	}
}

// checkEnvelope validates at the boundary; a foreign-schema envelope skips the op and entry checks, it is only held.
func checkEnvelope(e Envelope, localSchema int) error {
	if !originRE.MatchString(e.Origin) || e.Seq <= 0 {
		return errors.New("memory envelopes: origin must be plain text and origin_seq must be positive")
	}
	if heldSchema(e, localSchema) {
		return nil
	}
	if e.Schema != OpSchema(e.Op) {
		return fmt.Errorf("memory envelopes: %q/%d: op %q does not travel on schema %d", e.Origin, e.Seq, e.Op, e.Schema)
	}
	if e.Op == EnvelopeOpOrder {
		if strings.TrimSpace(e.DocRef) == "" || e.EntryRef != "" {
			return fmt.Errorf("memory envelopes: %q/%d: an order change needs doc_ref and no entry_ref", e.Origin, e.Seq)
		}
		return nil
	}
	if e.Op != EnvelopeOpUpsert && e.Op != EnvelopeOpDelete {
		return fmt.Errorf("memory envelopes: %q/%d: unknown op %q", e.Origin, e.Seq, e.Op)
	}
	if strings.TrimSpace(e.EntryRef) == "" || strings.ContainsAny(e.EntryRef, "\r\n\t") {
		return fmt.Errorf("memory envelopes: %q/%d: entry_ref is required without line breaks", e.Origin, e.Seq)
	}
	return nil
}

// heldSchema: a schema this reader does not know (newer than localSchema, or older than the first) is only held.
func heldSchema(e Envelope, localSchema int) bool {
	return e.Schema > localSchema || e.Schema < EnvelopeSchema
}

func importOrigin(st EnvelopeStore, origin string, rows []Envelope, localSchema int, rep *ImportReport) error {
	acked, _, err := st.Watermark(origin)
	if err != nil {
		return fmt.Errorf("memory envelopes: watermark %q: %w", origin, err)
	}
	for _, e := range rows {
		if e.Seq > acked+1 {
			rep.HeldOrigins = append(rep.HeldOrigins, origin)
			rep.Held = append(rep.Held, fmt.Sprintf("%s: gap: have %d, got %d; the sender resends from %d", origin, acked, e.Seq, acked))
			return nil
		}
		if e.Seq == acked+1 && heldSchema(e, localSchema) {
			rep.HeldOrigins = append(rep.HeldOrigins, origin)
			action := "peer must upgrade; run on peer: claude-code upgrade"
			if e.Schema > localSchema {
				action = "run: claude-code upgrade"
			}
			rep.Held = append(rep.Held, fmt.Sprintf("%s/%d: schema: local %d, peer %d; %s", origin, e.Seq, localSchema, e.Schema, action))
			return nil
		}
		if e.Seq <= acked && heldSchema(e, localSchema) {
			rep.Dup++
			continue
		}
		conflict, err := st.ApplyEnvelope(e)
		if err != nil {
			return fmt.Errorf("memory envelopes: apply %q/%d: %w", origin, e.Seq, err)
		}
		countApplied(rep, e.Seq <= acked, conflict)
		acked = max(acked, e.Seq)
	}
	return nil
}

// countApplied files one result: a repeat counts as a duplicate unless it filed a seq-mismatch conflict.
func countApplied(rep *ImportReport, repeat, conflict bool) {
	switch {
	case conflict:
		rep.Conflicts++
		if !repeat {
			rep.Applied++
		}
	case repeat:
		rep.Dup++
	default:
		rep.Applied++
	}
}
