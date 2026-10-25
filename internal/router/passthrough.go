package router

import (
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
	case providers.DialectOpenAI, providers.DialectResponses, providers.DialectGemini:
		return parts[0], parts[1], parts[2], true
	}
	return "", "", "", false
}

// passthrough forwards a same-dialect request verbatim to the provider's route with egress auth.
func (r *Router) passthrough(w http.ResponseWriter, in *http.Request, provider, dialect, rest string) {
	start := r.now()
	sw := &statusWriter{ResponseWriter: w, rc: http.NewResponseController(w)}
	defer func() {
		fmt.Fprintf(r.log, "%s %s %s %d %d %s\n", start.Format(time.RFC3339), provider, "-", sw.status, 1, time.Since(start).Round(time.Millisecond))
	}()
	if in.Method != http.MethodPost && in.Method != http.MethodGet {
		writeError(sw, http.StatusNotFound, "router: unsupported route "+in.Method+" "+in.URL.Path+" (passthrough takes GET or POST)")
		return
	}
	reg, err := r.registry()
	if err != nil {
		writeError(sw, http.StatusInternalServerError, "router: "+err.Error())
		return
	}
	p, err := reg.Get(provider)
	if err != nil {
		writeError(sw, http.StatusBadRequest, "router: "+err.Error())
		return
	}
	route, ok := p.Route(dialect)
	if !ok {
		writeError(sw, http.StatusNotFound, "router: provider "+p.Name+" has no "+dialect+" route")
		return
	}
	var body []byte
	if in.Method == http.MethodPost {
		body, err = io.ReadAll(io.LimitReader(in.Body, maxBody+1))
		if err != nil {
			writeError(sw, http.StatusBadRequest, "router: read body: "+err.Error())
			return
		}
		if len(body) > maxBody {
			writeError(sw, http.StatusRequestEntityTooLarge, "router: request body over 64MiB")
			return
		}
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
		writeError(sw, http.StatusBadGateway, "router: "+p.Name+": "+err.Error())
		return
	}
	relay(sw, resp)
}
