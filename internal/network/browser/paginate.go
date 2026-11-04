package browser

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

// paginate loads the start page and collects rows by the chosen mode; rows gathered so far return with any error.
func paginate(ctx context.Context, env Env, c *cdpConn, gate *robotsGate, o ExtractOptions, start, js string) ([]Row, error) {
	if err := c.Navigate(ctx, start); err != nil {
		return nil, err
	}
	switch {
	case o.Scroll || o.LoadMore != "":
		return growPage(ctx, env, c, o, js)
	case o.Template != "":
		return templatePages(ctx, env, c, gate, o, js)
	}
	return nextPages(ctx, env, c, gate, o, js)
}

// nextPages collects the current page, then follows the next control until the rows repeat or run out.
func nextPages(ctx context.Context, env Env, c *cdpConn, gate *robotsGate, o ExtractOptions, js string) ([]Row, error) {
	var all, prev []Row
	for page := 1; ; page++ {
		rows, err := grab(ctx, c, js)
		if err != nil {
			return all, err
		}
		if page == 1 && len(rows) == 0 {
			return nil, fmt.Errorf("container %q matched nothing within 10s", o.Container)
		}
		if prev != nil && reflect.DeepEqual(prev, rows) {
			return all, nil
		}
		prev = rows
		all = append(all, rows...)
		if o.noMorePages(page) {
			return all, nil
		}
		if more, err := advance(ctx, env, c, gate, o); err != nil || !more {
			return all, err
		}
	}
}

// noMorePages reports whether next-link pagination ends after page: no --next control or the page cap reached.
func (o ExtractOptions) noMorePages(page int) bool { return o.Next == "" || page >= o.MaxPages }

// advance waits politely, then follows the next control; false when there is no further page.
func advance(ctx context.Context, env Env, c *cdpConn, gate *robotsGate, o ExtractOptions) (bool, error) {
	env.politeDelay()
	return goNext(ctx, env, c, gate, o)
}

// templatePages walks {page} URLs from 2 up to MaxPages, stopping at the first empty page.
func templatePages(ctx context.Context, env Env, c *cdpConn, gate *robotsGate, o ExtractOptions, js string) ([]Row, error) {
	var all []Row
	for page := 1; page <= o.MaxPages; page++ {
		if page > 1 {
			env.politeDelay()
			u := strings.ReplaceAll(o.Template, "{page}", strconv.Itoa(page))
			if err := gate.check(ctx, u); err != nil {
				return all, err
			}
			if err := c.Navigate(ctx, u); err != nil {
				return all, err
			}
		}
		rows, err := grab(ctx, c, js)
		if err != nil {
			return all, err
		}
		if len(rows) == 0 {
			if page == 1 {
				return nil, fmt.Errorf("container %q matched nothing within 10s", o.Container)
			}
			return all, nil
		}
		all = append(all, rows...)
	}
	return all, nil
}

// goNext follows the next control; false when it is absent or disabled.
func goNext(ctx context.Context, env Env, c *cdpConn, gate *robotsGate, o ExtractOptions) (bool, error) {
	var st struct {
		State, Href, Sig string
	}
	probe := `(()=>{const e=document.querySelector(` + jsString(o.Next) + `);
if(!e)return{state:'gone'};
if(e.disabled||e.getAttribute('aria-disabled')==='true'||/\bdisabled\b/.test(e.className))return{state:'disabled'};
return{state:'ok',href:(e.tagName==='A'&&e.href&&e.href.split('#')[0]!==location.href.split('#')[0]&&e.href)||'',sig:` + sigJS(o.Container) + `};})()`
	if err := c.Eval(ctx, probe, &st); err != nil {
		return false, err
	}
	if st.State != "ok" {
		return false, nil
	}
	if st.Href != "" && !strings.HasPrefix(st.Href, "javascript:") {
		if err := gate.check(ctx, st.Href); err != nil {
			return false, err
		}
		return true, c.Navigate(ctx, st.Href)
	}
	if err := c.Eval(ctx, `document.querySelector(`+jsString(o.Next)+`).click()`, nil); err != nil {
		return false, err
	}
	return waitChange(ctx, env, c, `(`+sigJS(o.Container)+`)`, st.Sig)
}

// sigJS fingerprints the page: URL, container count and the first container's text.
func sigJS(container string) string {
	q := jsString(container)
	return `location.href+'|'+document.querySelectorAll(` + q + `).length+'|'+((document.querySelector(` + q + `)||{}).textContent||'').slice(0,200)`
}

// waitChange polls until the expression differs from before (true) or 10s pass (false).
func waitChange(ctx context.Context, env Env, c *cdpConn, expr, before string) (bool, error) {
	var lastErr error
	for range 50 {
		env.sleep(200 * time.Millisecond)
		var now string
		if lastErr = c.Eval(ctx, expr, &now); lastErr == nil && now != before {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
	}
	return false, lastErr
}

// growPage scrolls (or clicks load-more) until the container count stops growing, then extracts once.
func growPage(ctx context.Context, env Env, c *cdpConn, o ExtractOptions, js string) ([]Row, error) {
	count := `document.querySelectorAll(` + jsString(o.Container) + `).length`
	var prev int
	if err := c.Eval(ctx, `(async()=>{const end=Date.now()+10000;
while(!`+count+`&&Date.now()<end)await new Promise(r=>setTimeout(r,200));return `+count+`})()`, &prev); err != nil {
		return nil, err
	}
	if prev == 0 {
		return nil, fmt.Errorf("container %q matched nothing within 10s", o.Container)
	}
	stalls := 0
	for round := 0; round < o.MaxPages && stalls < 3; round++ {
		step, err := growStep(ctx, env, c, o, count, &prev)
		if err != nil {
			return nil, err
		}
		if step.stop {
			break
		}
		if step.clicked {
			stalls = 2 // one no-growth round after a click ends the loop
		}
		if step.grew {
			stalls = 0
		} else {
			stalls++
		}
	}
	return grab(ctx, c, js)
}

// growResult reports one grow round: stop when load-more is gone, grew when the count rose, clicked after a click.
type growResult struct{ stop, grew, clicked bool }

// growStep runs one scroll or load-more round and records the new container count in prev.
func growStep(ctx context.Context, env Env, c *cdpConn, o ExtractOptions, count string, prev *int) (growResult, error) {
	var res growResult
	if o.LoadMore != "" {
		var clicked bool
		if err := c.Eval(ctx, `(()=>{const b=document.querySelector(`+jsString(o.LoadMore)+`);
if(!b||b.disabled||b.offsetParent===null)return false;b.click();return true})()`, &clicked); err != nil {
			return res, err
		}
		if !clicked {
			res.stop = true
			return res, nil
		}
		res.clicked = true
	} else if err := c.Eval(ctx, `window.scrollTo(0,document.body.scrollHeight)`, nil); err != nil {
		return res, err
	}
	env.politeDelay()
	var n int
	if err := c.Eval(ctx, count, &n); err != nil {
		return res, err
	}
	if n > *prev {
		*prev, res.grew = n, true
	}
	return res, nil
}

// jsString renders s as a JS string literal via JSON.
func jsString(s string) string {
	out, _ := textutil.MarshalNoEscape(s)
	return string(out)
}
