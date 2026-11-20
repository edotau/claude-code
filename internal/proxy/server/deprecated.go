package server

import (
	"fmt"
	"net/http"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
)

// noteDeprecated records the endpoint a Databricks 400 names as deprecated in the picker's drop list, off the request path.
func (r *Router) noteDeprecated(p *providers.Provider, resp *http.Response) {
	if p.Preset != providers.PresetDatabricks || resp.StatusCode != http.StatusBadRequest {
		return
	}
	id, ok := serving.DeprecatedEndpoint(resp.StatusCode, string(peekBody(resp, 4<<10)))
	if !ok {
		return
	}
	row := r.registryRow(p.Name) // the picker scope hashes the registry row, never a routed copy
	if row == nil {
		return
	}
	key := p.Name + ":" + id
	r.mu.Lock()
	seen := r.deprecated[key]
	r.deprecated[key] = true
	r.mu.Unlock()
	if seen {
		return
	}
	goStateWrite(r, func() {
		added, err := providers.RecordDeprecated(row, id, r.now())
		switch {
		case err != nil:
			r.mu.Lock()
			delete(r.deprecated, key) // a failed write is retried on the next such reply
			r.mu.Unlock()
			fmt.Fprintf(r.log, "proxy: record %s deprecated on %s: %v\n", id, p.Name, err)
		case added:
			fmt.Fprintf(r.log, "proxy: %s answered %s is deprecated; dropped from the picker and rotation pool\n", p.Name, id)
		}
	})
}

// registryRow is the registry's row for name, the same value the CLI's readers hash; nil when it is gone.
func (r *Router) registryRow(name string) *providers.Provider {
	reg, err := r.registry()
	if err != nil {
		return nil
	}
	return reg.Providers[name]
}
