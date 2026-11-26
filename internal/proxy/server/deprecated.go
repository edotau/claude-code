package server

import (
	"fmt"
	"net/http"

	"github.com/edotau/claude-code/internal/catalog"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
)

// noteUnserved drops the endpoint a Databricks 400 (deprecated) or 403 (gateway-only) names, off the request path.
func (r *Router) noteUnserved(p *providers.Provider, resp *http.Response) {
	if p.Preset != providers.PresetDatabricks || (resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusForbidden) {
		return
	}
	id, reason, ok := serving.UnservedEndpoint(resp.StatusCode, string(peekBody(resp, 4<<10)))
	if !ok {
		return
	}
	reg, err := r.registry()
	if err != nil || reg.Providers[p.Name] == nil {
		return
	}
	row := reg.Providers[p.Name] // the picker scope hashes the registry row, never a routed copy
	key := p.Name + ":" + id
	r.mu.Lock()
	seen := r.deprecated[key]
	r.deprecated[key] = true
	r.mu.Unlock()
	if seen {
		return
	}
	goStateWrite(r, func() {
		added, err := catalog.RecordDropped(row, id, reason, r.now())
		switch {
		case err != nil:
			r.mu.Lock()
			delete(r.deprecated, key) // a failed write is retried on the next such reply
			r.mu.Unlock()
			fmt.Fprintf(r.log, "proxy: record %s %s on %s: %v\n", id, reason, p.Name, err)
		case added:
			fmt.Fprintf(r.log, "proxy: %s answered %s is %s; dropped from the picker and rotation pool\n", p.Name, id, reason)
		}
	})
}
