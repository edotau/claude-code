package browser

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
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
	var all []Row
	for page := 1; ; page++ {
		rows, err := grab(ctx, c, js)
		if err != nil {
			return all, err
		}
		if page == 1 && len(rows) == 0 {
			return nil, fmt.Errorf("container %q matched nothing within 10s", o.Container)
		}
		all = append(all, rows...)
		if o.Next == "" || page >= o.MaxPages {
			return all, nil
		}
		env.politeDelay()
		moved, err := goNext(ctx, env, c, gate, o)
		if err != nil || !moved {
			return all, err
		}
	}
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
return{state:'ok',href:(e.tagName==='A'&&e.href)||'',sig:` + sigJS(o.Container) + `};})()`
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
	for range 50 {
		env.sleep(200 * time.Millisecond)
		var now string
		if err := c.Eval(ctx, expr, &now); err == nil && now != before {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
	}
	return false, nil
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
		if o.LoadMore != "" {
			var clicked bool
			if err := c.Eval(ctx, `(()=>{const b=document.querySelector(`+jsString(o.LoadMore)+`);
if(!b||b.disabled||b.offsetParent===null)return false;b.click();return true})()`, &clicked); err != nil {
				return nil, err
			}
			if !clicked {
				break
			}
			stalls = 2 // one no-growth round after a click ends the loop
		} else if err := c.Eval(ctx, `window.scrollTo(0,document.body.scrollHeight)`, nil); err != nil {
			return nil, err
		}
		env.politeDelay()
		var n int
		if err := c.Eval(ctx, count, &n); err != nil {
			return nil, err
		}
		if n > prev {
			prev, stalls = n, 0
		} else {
			stalls++
		}
	}
	return grab(ctx, c, js)
}

// jsString renders s as a JS string literal via JSON.
func jsString(s string) string {
	out, _ := marshalPlain(s)
	return out
}
