// Package sqlexec runs one statement through the Databricks SQL Statement Execution API on a profile's workspace.
package sqlexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/httpx"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	statementsPath = "/api/2.0/sql/statements"
	rowLimit       = 10000
	pollEvery      = 2 * time.Second
	maxWait        = 2 * time.Minute
	cancelWait     = 10 * time.Second
	attempts       = 3 // 429 and 503 are retried twice
	retryBase      = 500 * time.Millisecond
	replyLimit     = 32 << 20
	messageRunes   = 160
	maxCells       = 1_000_000 // across every chunk
	maxBytes       = 64 << 20  // cell text across every chunk
	maxChunks      = 100
)

var (
	statementIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	urlRE         = regexp.MustCompile(`(?i)https?://[^\s"'<>]*[^\s"'<>.,:;)]`)
)

// httpClient outlives the 30s server-side wait_timeout so the POST is not cut first; a var so tests trust a TLS fake.
var httpClient = func() *http.Client {
	c := *http.DefaultClient
	c.Timeout = 45 * time.Second
	return &c
}

// NullCell marks a SQL NULL in Result.Rows, so a caller can tell it from an empty string.
const NullCell = "\x00null"

// Result is a statement's column names and rows; NULL cells are NullCell. Truncated means rows were dropped by a cap.
type Result struct {
	Columns   []string
	Rows      [][]string
	Truncated bool
}

type statement struct {
	ID     string `json:"statement_id"`
	Status struct {
		State string `json:"state"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"status"`
	Manifest struct {
		Schema struct {
			Columns []struct {
				Name string `json:"name"`
			} `json:"columns"`
		} `json:"schema"`
		Truncated bool `json:"truncated"`
	} `json:"manifest"`
	Result chunk `json:"result"`
}

type chunk struct {
	Index int         `json:"chunk_index"`
	Data  [][]*string `json:"data_array"`
	Next  *int        `json:"next_chunk_index"`
}

type client struct {
	p    *providers.Provider
	base string
}

// Run executes sql on warehouseID, polling up to two minutes, and returns at most rowLimit rows.
// Every error it returns is scrubbed of the workspace host, URLs and secret-shaped text; errors.Is/As still see the cause.
func Run(ctx context.Context, p *providers.Provider, warehouseID, sql string) (Result, error) {
	res, err := execute(ctx, p, warehouseID, sql)
	if err != nil {
		return Result{}, &scrubbedError{msg: scrub(err.Error(), p.Host), err: err}
	}
	return res, nil
}

type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// scrub drops URLs and the profile's host from s, then credential-shaped runs.
func scrub(s, host string) string {
	s = urlRE.ReplaceAllString(s, "<url>")
	if u, err := url.Parse(host); err == nil && u.Hostname() != "" {
		for _, h := range []string{u.Host, u.Hostname()} {
			s = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(h)).ReplaceAllString(s, "<host>")
		}
	}
	return textutil.RedactSecrets(s)
}

func execute(ctx context.Context, p *providers.Provider, warehouseID, sql string) (Result, error) {
	host := providers.PresetHost(p.Host)
	if host == "" {
		return Result{}, errors.New("sql statement: profile has no workspace host")
	}
	if !providers.ValidWarehouseID(warehouseID) {
		return Result{}, errors.New("sql statement: warehouse id must be 1-64 alphanumeric characters")
	}
	body, err := json.Marshal(map[string]any{"statement": sql, "warehouse_id": warehouseID, "wait_timeout": "30s",
		"on_wait_timeout": "CONTINUE", "disposition": "INLINE", "format": "JSON_ARRAY", "row_limit": rowLimit})
	if err != nil {
		return Result{}, err
	}
	ctx, done := context.WithTimeout(ctx, maxWait)
	defer done()
	c := &client{p: p, base: host + statementsPath}
	var st statement
	if err := c.call(ctx, http.MethodPost, "", body, &st); err != nil {
		return Result{}, err
	}
	if !statementIDRE.MatchString(st.ID) {
		return Result{}, errors.New("sql statement: the server returned an invalid statement id")
	}
	if st, err = c.wait(ctx, st); err != nil {
		return Result{}, err
	}
	return c.collect(ctx, st)
}

// wait polls a PENDING or RUNNING statement until it ends, cancelling it when ctx or the bound ends first.
func (c *client) wait(ctx context.Context, st statement) (statement, error) {
	for st.Status.State == "PENDING" || st.Status.State == "RUNNING" {
		if err := textutil.RetrySleep(ctx, pollEvery); err != nil {
			return st, c.cancel(ctx, st.ID)
		}
		var next statement
		if err := c.call(ctx, http.MethodGet, "/"+url.PathEscape(st.ID), nil, &next); err != nil {
			if ctx.Err() != nil {
				return st, c.cancel(ctx, st.ID)
			}
			return st, err
		}
		next.ID = st.ID
		st = next
	}
	switch st.Status.State {
	case "SUCCEEDED":
		return st, nil
	case "FAILED", "CANCELED", "CLOSED":
		msg := textutil.CapRunes(strings.TrimSpace(st.Status.Error.Message), messageRunes, "…")
		return st, fmt.Errorf("sql statement %s: %s", strings.ToLower(st.Status.State), msg)
	}
	return st, fmt.Errorf("sql statement: unexpected state %q", textutil.CapRunes(st.Status.State, 40, "…"))
}

// cancel posts a best-effort cancel on a fresh short context and returns the timeout error.
func (c *client) cancel(ctx context.Context, id string) error {
	cctx, done := context.WithTimeout(context.WithoutCancel(ctx), cancelWait)
	defer done()
	if err := c.call(cctx, http.MethodPost, "/"+url.PathEscape(id)+"/cancel", nil, nil); err != nil {
		return fmt.Errorf("sql statement timed out (cancel failed: %v): %w", err, ctx.Err())
	}
	return fmt.Errorf("sql statement timed out: %w", ctx.Err())
}

// collect gathers the inline rows, then follows next_chunk_index until a cap (rows, cells, bytes, chunks) is reached.
func (c *client) collect(ctx context.Context, st statement) (Result, error) {
	res := Result{Truncated: st.Manifest.Truncated}
	for _, col := range st.Manifest.Schema.Columns {
		res.Columns = append(res.Columns, col.Name)
	}
	var used budget
	cur, ch := st.Result.Index, st.Result
	for fetched := 0; ; fetched++ {
		var full bool
		res.Rows, full = appendRows(res.Rows, ch.Data, &used)
		if full || len(res.Rows) >= rowLimit || ch.Next != nil && fetched+1 >= maxChunks {
			return capRows(res, full, ch.Next != nil), nil
		}
		if ch.Next == nil {
			return res, nil
		}
		// a non-advancing index would loop until the bound
		if *ch.Next <= cur {
			return Result{}, fmt.Errorf("sql statement: chunk %d points back to %d", cur, *ch.Next)
		}
		cur = *ch.Next
		var next chunk
		if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/%s/result/chunks/%d", url.PathEscape(st.ID), cur), nil, &next); err != nil {
			return Result{}, err
		}
		ch = next
	}
}

// capRows trims res to rowLimit and marks it Truncated when a cap or a pending chunk dropped rows.
func capRows(res Result, full, more bool) Result {
	res.Truncated = res.Truncated || full || len(res.Rows) > rowLimit || more
	res.Rows = res.Rows[:min(len(res.Rows), rowLimit)]
	return res
}

type budget struct{ cells, bytes int }

// appendRows adds data to rows until the cell or byte budget would be exceeded; full reports that it stopped early.
func appendRows(rows [][]string, data [][]*string, b *budget) (out [][]string, full bool) {
	for _, d := range data {
		row := make([]string, len(d))
		size := 0
		for i, v := range d {
			row[i] = NullCell
			if v != nil {
				row[i] = *v
				size += len(*v)
			}
		}
		if b.cells+len(d) > maxCells || b.bytes+size > maxBytes {
			return rows, true
		}
		b.cells, b.bytes = b.cells+len(d), b.bytes+size
		rows = append(rows, row)
	}
	return rows, false
}

// call sends one request, retrying 429 and 503 honouring Retry-After, and decodes a 2xx reply into out.
func (c *client) call(ctx context.Context, method, path string, body []byte, out any) error {
	for attempt := 1; ; attempt++ {
		status, data, rh, err := c.send(ctx, method, path, body)
		if status == 0 {
			return err
		}
		if (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) && attempt < attempts {
			if werr := textutil.WaitRetry(ctx, attempt, retryBase, rh.Get("Retry-After")); werr != nil {
				return fmt.Errorf("sql statement: %w", werr)
			}
			continue
		}
		return decode(status, data, err, out)
	}
}

// decode turns a final reply into an error, or a 2xx body into out (nil out skips decoding).
func decode(status int, data []byte, err error, out any) error {
	switch {
	case status/100 != 2:
		return statusError(status, data)
	case errors.Is(err, httpx.ErrTooLarge):
		return errors.New("sql statement: reply too large")
	case err != nil:
		return errors.New("sql statement: reply read failed")
	case out == nil:
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("sql statement: decode reply: %w", err)
	}
	return nil
}

// send is one authorised request; a status-0 error never carries the URL, host or a header.
func (c *client) send(ctx context.Context, method, path string, body []byte) (int, []byte, http.Header, error) {
	hdr := http.Header{"Accept": {"application/json"}}
	if body != nil {
		hdr.Set("Content-Type", "application/json")
	}
	var authErr error
	status, data, rh, err := httpx.Do(ctx, httpx.Call{Client: httpClient(), Method: method, URL: c.base + path,
		Header: hdr, Body: body, Limit: replyLimit, NoRedirect: true,
		Authorize: func(h http.Header) error { authErr = providers.Authorize(ctx, c.p, h); return authErr }})
	switch {
	case authErr != nil:
		return 0, nil, nil, fmt.Errorf("sql statement: authorize: %w", authErr)
	case status == 0 && ctx.Err() != nil:
		return 0, nil, nil, fmt.Errorf("sql statement: %w", ctx.Err())
	case status == 0:
		return 0, nil, nil, transportError(err)
	}
	return status, data, rh, err
}

// transportError names the failed step only: *url.Error and *net.OpError both embed the address.
func transportError(err error) error {
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return errors.New("sql statement: request timed out")
	}
	if op, ok := errors.AsType[*net.OpError](err); ok {
		return fmt.Errorf("sql statement: %s failed", op.Op)
	}
	return errors.New("sql statement: request failed")
}

// statusError keeps the status and the API's error code and message, cut to messageRunes.
func statusError(status int, body []byte) error {
	var doc struct {
		Code    string `json:"error_code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &doc) // a non-JSON body leaves the status alone
	msg := strings.TrimSpace(strings.Join([]string{doc.Code, doc.Message}, " "))
	if msg == "" {
		return fmt.Errorf("sql statement: HTTP %d", status)
	}
	return fmt.Errorf("sql statement: HTTP %d: %s", status, textutil.CapRunes(msg, messageRunes, "…"))
}
