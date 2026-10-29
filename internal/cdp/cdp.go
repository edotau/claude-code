package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"time"
)

// DefaultBase mirrors browser.CDPPort (browser imports this package, so it cannot be imported back).
const DefaultBase = "http://127.0.0.1:9222"

// ErrNoBrowser means nothing answered on the CDP endpoint.
var ErrNoBrowser = errors.New(noBrowserHint())

// noBrowserHint names the fix per OS: setup launches CDP only on WSL; elsewhere the user starts the browser.
func noBrowserHint() string {
	if runtime.GOOS == "darwin" {
		return "no browser answers on the CDP port; quit Chrome and run " +
			"`open -a 'Google Chrome' --args --remote-debugging-port=9222`"
	}
	return "no browser answers on the CDP port; run `claude-code network browser --setup`"
}

// Conn is one CDP session bound to a tab this package opened.
type Conn struct {
	ws, base, id string
	sock         *wsConn
	mu           sync.Mutex
	next         int
	pending      map[int]chan response
	waiters      map[string][]chan json.RawMessage
	err          error
	done         chan struct{}
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type message struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	response
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// Dial opens a new about:blank tab through base ("" = DefaultBase) and connects to it.
func Dial(ctx context.Context, base string) (*Conn, error) {
	if base == "" {
		base = DefaultBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/json/new?about:blank", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNoBrowser, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tab struct{ ID, WebSocketDebuggerURL string }
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &tab) != nil || tab.WebSocketDebuggerURL == "" {
		return nil, fmt.Errorf("cdp: new tab failed (%s)", resp.Status)
	}
	c := &Conn{base: base, id: tab.ID, pending: map[int]chan response{},
		waiters: map[string][]chan json.RawMessage{}, done: make(chan struct{})}
	if c.sock, err = wsDial(ctx, tab.WebSocketDebuggerURL); err != nil {
		c.closeTab()
		return nil, err
	}
	go c.readLoop()
	return c, nil
}

// readLoop routes responses by id and events by method until the socket ends.
func (c *Conn) readLoop() {
	defer close(c.done)
	for {
		raw, err := c.sock.readMessage()
		if err != nil {
			c.mu.Lock()
			c.err = err
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		var m message
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		c.mu.Lock()
		if m.ID != 0 {
			if ch, ok := c.pending[m.ID]; ok {
				ch <- m.response
				delete(c.pending, m.ID)
			}
		} else {
			for _, ch := range c.waiters[m.Method] {
				ch <- m.Params
			}
			delete(c.waiters, m.Method)
		}
		c.mu.Unlock()
	}
}

// waitEvent registers a one-shot waiter for method; register before the command that triggers it.
func (c *Conn) waitEvent(method string) chan json.RawMessage {
	ch := make(chan json.RawMessage, 1)
	c.mu.Lock()
	c.waiters[method] = append(c.waiters[method], ch)
	c.mu.Unlock()
	return ch
}

// call sends one command and waits for its response.
func (c *Conn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ch := make(chan response, 1)
	c.mu.Lock()
	c.next++
	id := c.next
	c.pending[id] = ch
	c.mu.Unlock()
	b, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err == nil {
		err = c.sock.writeFrame(opText, b)
	}
	if err != nil {
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("cdp: connection closed: %v", c.err)
		}
		if r.Error != nil {
			return nil, fmt.Errorf("cdp %s: %s", method, r.Error.Message)
		}
		return r.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Navigate loads url and waits for Page.loadEventFired.
func (c *Conn) Navigate(ctx context.Context, target string) error {
	if _, err := c.call(ctx, "Page.enable", map[string]any{}); err != nil {
		return err
	}
	loaded := c.waitEvent("Page.loadEventFired")
	res, err := c.call(ctx, "Page.navigate", map[string]any{"url": target})
	if err != nil {
		return err
	}
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	if json.Unmarshal(res, &nav) == nil && nav.ErrorText != "" {
		return fmt.Errorf("cdp: navigate %s: %s", target, nav.ErrorText)
	}
	select {
	case <-loaded:
		return nil
	case <-c.done:
		return fmt.Errorf("cdp: connection closed: %v", c.err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Eval runs expr in the page (awaiting promises) and decodes the by-value result into out (nil = discard).
func (c *Conn) Eval(ctx context.Context, expr string, out any) error {
	res, err := c.call(ctx, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true})
	if err != nil {
		return err
	}
	var r struct {
		Result  struct{ Value json.RawMessage } `json:"result"`
		Details *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return err
	}
	if d := r.Details; d != nil {
		msg := d.Exception.Description
		if msg == "" {
			msg = d.Text
		}
		return fmt.Errorf("cdp eval: %s", msg)
	}
	if out == nil || len(r.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(r.Result.Value, out)
}

// Close shuts the socket and closes the tab.
func (c *Conn) Close() error {
	err := c.sock.close()
	c.closeTab()
	return err
}

func (c *Conn) closeTab() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/json/close/"+url.PathEscape(c.id), nil)
	if err != nil {
		return
	}
	if resp, err := httpClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}
