package browser

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/cdp"
)

// Field is one extracted column: Selector inside the container, Attr "" for text content.
type Field struct{ Name, Selector, Attr string }

// ExtractOptions drives Extract; the pagination fields are mutually exclusive.
type ExtractOptions struct {
	URL, Container           string
	CDPBase                  string // debug endpoint; "" = cdp.DefaultBase (tests point it at a fake)
	Fields                   []Field
	Next, Template, LoadMore string
	Scroll                   bool
	MaxPages                 int
	Key, Format              string
	IgnoreRobots             bool
}

// ParseField parses "name=selector" or "name=selector@attr".
func ParseField(s string) (Field, error) {
	name, sel, ok := strings.Cut(s, "=")
	if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(sel) == "" {
		return Field{}, fmt.Errorf("--field %q: want name=selector or name=selector@attr", s)
	}
	f := Field{Name: strings.TrimSpace(name), Selector: strings.TrimSpace(sel)}
	if i := strings.LastIndexByte(f.Selector, '@'); i > 0 && !strings.ContainsAny(f.Selector[i+1:], " []()=\"'>+~,.#:") && i+1 < len(f.Selector) {
		f.Selector, f.Attr = strings.TrimSpace(f.Selector[:i]), f.Selector[i+1:]
	}
	return f, nil
}

// Validate checks the option combination before any browser work.
func (o ExtractOptions) Validate() error {
	if o.URL == "" || o.Container == "" || len(o.Fields) == 0 {
		return errors.New("--extract needs a URL, --container and at least one --field")
	}
	modes := 0
	for _, set := range []bool{o.Next != "", o.Template != "", o.Scroll, o.LoadMore != ""} {
		if set {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("use at most one of --next, --pages, --scroll, --load-more")
	}
	if o.Template != "" && !strings.Contains(o.Template, "{page}") {
		return errors.New("--pages needs a {page} placeholder")
	}
	seen := map[string]bool{}
	for _, f := range o.Fields {
		if seen[f.Name] {
			return fmt.Errorf("duplicate --field name %q", f.Name)
		}
		seen[f.Name] = true
	}
	if o.Key != "" && !seen[o.Key] {
		return fmt.Errorf("--key %q is not a --field name", o.Key)
	}
	switch o.Format {
	case "jsonl", "csv", "json":
	default:
		return fmt.Errorf("--format %q: want jsonl, csv or json in extract mode", o.Format)
	}
	return nil
}

// Row holds one record's cleaned values in Fields order; nil is null.
type Row []*string

// extractJS builds the page expression; all user input travels as one JSON literal, never as code.
func extractJS(container string, fields []Field) (string, error) {
	type jf struct{ N, S, A string }
	cfg := struct {
		C string
		F []jf
	}{C: container}
	for _, f := range fields {
		cfg.F = append(cfg.F, jf{f.Name, f.Selector, f.Attr})
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return `(async()=>{const cfg=` + string(b) + `;
const end=Date.now()+10000;
while(!document.querySelector(cfg.C)&&Date.now()<end)await new Promise(r=>setTimeout(r,200));
return [...document.querySelectorAll(cfg.C)].map(el=>cfg.F.map(f=>{
const t=el.querySelector(f.S);if(!t)return null;
if(!f.A)return t.textContent;
const p=(f.A==='href'||f.A==='src')?t[f.A]:null;
return typeof p==='string'&&p!==''?p:t.getAttribute(f.A);}));})()`, nil
}

// clean collapses whitespace, strips zero-width characters and maps empty to nil.
func clean(s *string) *string {
	if s == nil {
		return nil
	}
	z := strings.Map(func(r rune) rune {
		if r == 0x200B || r == 0x200C || r == 0x200D || r == 0xFEFF {
			return -1
		}
		return r
	}, *s)
	out := strings.Join(strings.FieldsFunc(z, unicode.IsSpace), " ")
	if out == "" {
		return nil
	}
	return &out
}

// grab evaluates the extraction on the current page and cleans the rows.
func grab(ctx context.Context, c *cdp.Conn, js string) ([]Row, error) {
	var raw [][]*string
	if err := c.Eval(ctx, js, &raw); err != nil {
		return nil, err
	}
	rows := make([]Row, len(raw))
	for i, r := range raw {
		rows[i] = make(Row, len(r))
		for j, v := range r {
			rows[i][j] = clean(v)
		}
	}
	return rows, nil
}

// dedupe drops later rows repeating a non-null key value; rows with a null key are kept.
func dedupe(rows []Row, keyIdx int) []Row {
	if keyIdx < 0 {
		return rows
	}
	seen := map[string]bool{}
	var out []Row
	for _, r := range rows {
		if k := r[keyIdx]; k != nil {
			if seen[*k] {
				continue
			}
			seen[*k] = true
		}
		out = append(out, r)
	}
	return out
}

func (e Env) politeDelay() { e.sleep(time.Duration(500+rand.IntN(1001)) * time.Millisecond) }

// Extract runs the extraction in its own CDP tab (always closed) and writes the rows to w.
func Extract(ctx context.Context, env Env, o ExtractOptions, w io.Writer) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o.MaxPages <= 0 {
		o.MaxPages = 100
	}
	gate := newRobotsGate(o.IgnoreRobots)
	start := o.URL
	if o.Template != "" {
		start = strings.ReplaceAll(o.Template, "{page}", "1")
	}
	if err := gate.check(ctx, start); err != nil {
		return err
	}
	js, err := extractJS(o.Container, o.Fields)
	if err != nil {
		return err
	}
	c, err := cdp.Dial(ctx, o.CDPBase)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	rows, runErr := paginate(ctx, env, c, gate, o, start, js)
	keyIdx := -1
	for i, f := range o.Fields {
		if f.Name == o.Key {
			keyIdx = i
		}
	}
	if werr := writeRows(w, o, dedupe(rows, keyIdx)); werr != nil && runErr == nil {
		runErr = werr
	}
	return runErr
}

func writeRows(w io.Writer, o ExtractOptions, rows []Row) error {
	if o.Format == "csv" {
		cw := csv.NewWriter(w)
		head := make([]string, len(o.Fields))
		for i, f := range o.Fields {
			head[i] = f.Name
		}
		_ = cw.Write(head)
		for _, r := range rows {
			rec := make([]string, len(r))
			for i, v := range r {
				if v != nil {
					rec[i] = *v
				}
			}
			_ = cw.Write(rec)
		}
		cw.Flush()
		return cw.Error()
	}
	var sb strings.Builder
	for i, r := range rows {
		line, err := rowJSON(o.Fields, r)
		if err != nil {
			return err
		}
		switch {
		case o.Format == "jsonl":
			sb.WriteString(line + "\n")
		case i == 0:
			sb.WriteString("[\n  " + line)
		default:
			sb.WriteString(",\n  " + line)
		}
	}
	if o.Format == "json" {
		if len(rows) == 0 {
			sb.WriteString("[]\n")
		} else {
			sb.WriteString("\n]\n")
		}
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

// rowJSON renders one object with keys in --field order.
func rowJSON(fields []Field, r Row) (string, error) {
	parts := make([]string, len(fields))
	for i, f := range fields {
		k, err := marshalPlain(f.Name)
		if err != nil {
			return "", err
		}
		v, err := marshalPlain(r[i])
		if err != nil {
			return "", err
		}
		parts[i] = k + ":" + v
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}

// marshalPlain is json.Marshal without HTML escaping, so URLs keep their & characters.
func marshalPlain(v any) (string, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(sb.String(), "\n"), nil
}
