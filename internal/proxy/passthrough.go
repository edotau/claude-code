package proxy

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// noForward are inbound credentials and transport headers; egress auth is re-added by send.
var noForward = map[string]bool{
	"Authorization": true, "X-Api-Key": true, "X-Goog-Api-Key": true, "Host": true, "Accept-Encoding": true,
}

// parsePassthrough splits /p/<provider>/<openai|responses|gemini>/<rest>.
func parsePassthrough(path string) (provider, dialect, rest string, ok bool) {
	tail, ok := strings.CutPrefix(path, "/p/")
	if !ok {
		return "", "", "", false
	}
	parts := strings.SplitN(tail, "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[2] == "" {
		return "", "", "", false
	}
	switch parts[1] {
	case providers.ProtocolOpenAI, providers.ProtocolResponses, providers.ProtocolGemini:
		return parts[0], parts[1], parts[2], true
	}
	return "", "", "", false
}

// passthrough forwards a same-dialect request verbatim to the provider's route with egress auth.
func (r *Router) passthrough(w http.ResponseWriter, in *http.Request, provider, dialect, rest string) {
	start := r.now()
	sw := &statusWriter{ResponseWriter: w, rc: http.NewResponseController(w)}
	var model, extra string
	attempts := 0
	in = in.WithContext(context.WithValue(in.Context(), attemptsKey{}, &attempts))
	defer func() {
		fmt.Fprintf(r.log, "%s %s %s %d %d %s%s\n", start.Format(time.RFC3339), provider, cmp.Or(model, "-"), sw.status,
			attempts, time.Since(start).Round(time.Millisecond), extra)
	}()
	if in.Method != http.MethodPost && in.Method != http.MethodGet {
		writeError(sw, http.StatusNotFound, "proxy: unsupported route "+in.Method+" "+in.URL.Path+" (passthrough takes GET or POST)")
		return
	}
	reg, err := r.registry()
	if err != nil {
		writeError(sw, http.StatusInternalServerError, "proxy: "+err.Error())
		return
	}
	p, err := reg.Get(provider)
	if err != nil {
		writeError(sw, http.StatusBadRequest, "proxy: "+err.Error())
		return
	}
	route, ok := p.Route(dialect)
	if !ok {
		writeError(sw, http.StatusNotFound, "proxy: provider "+p.Name+" has no "+dialect+" route")
		return
	}
	var body []byte
	if in.Method == http.MethodPost {
		body, err = io.ReadAll(io.LimitReader(in.Body, maxBody+1))
		if err != nil {
			writeError(sw, http.StatusBadRequest, "proxy: read body: "+err.Error())
			return
		}
		if len(body) > maxBody {
			writeError(sw, http.StatusRequestEntityTooLarge, "proxy: request body over 64MiB")
			return
		}
		model = passthroughModel(body)
	}
	target := route + "/" + rest
	if in.URL.RawQuery != "" {
		target += "?" + in.URL.RawQuery
	}
	resp, err := r.sendMethod(in, in.Method, p, target, body, func(h http.Header) {
		for k, vs := range in.Header {
			ck := http.CanonicalHeaderKey(k)
			if !hopHeaders[ck] && !noForward[ck] {
				h[ck] = append([]string(nil), vs...)
			}
		}
	})
	if err != nil {
		writeError(sw, http.StatusBadGateway, "proxy: "+p.Name+": "+err.Error())
		return
	}
	extra = passthroughDiag(resp)
	relay(sw, resp)
}

// attemptsKey carries a passthrough's upstream-attempt counter into sendMethod (auth refresh replays count).
type attemptsKey struct{}

// passthroughModel is the request body's "model", or "" when absent or not JSON.
func passthroughModel(body []byte) string {
	var head struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &head)
	return head.Model
}

// passthroughDiag is the log suffix: upstream request id, Retry-After and, on an error, its sanitized code.
func passthroughDiag(resp *http.Response) string {
	var b strings.Builder
	for _, k := range []string{"X-Request-Id", "X-Databricks-Request-Id", "X-Databricks-Requestid", "Request-Id"} {
		if v := resp.Header.Get(k); v != "" {
			b.WriteString(" req=" + diagToken(v))
			break
		}
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		b.WriteString(" retry_after=" + diagToken(v))
	}
	if resp.StatusCode >= 400 {
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
		var e struct {
			Code  string `json:"error_code"`
			Error struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(peek, &e)
		if c := cmp.Or(e.Code, e.Error.Code, e.Error.Type); c != "" {
			b.WriteString(" err=" + diagToken(c))
		}
	}
	return b.String()
}

// diagToken keeps a log value to [A-Za-z0-9._-], at most 64 chars, so a header can't inject text into the log.
func diagToken(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '.' || r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return -1
	}, s)
	return s[:min(len(s), 64)]
}
