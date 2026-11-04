package proxy

import (
	"context"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// Status is the loopback proxy's status: the payload of `gateway proxy --format json` and bare `gateway`'s "proxy".
type Status struct {
	Running  bool     `json:"running"`
	PID      int      `json:"pid,omitempty"`
	URL      string   `json:"url,omitempty"`
	Started  string   `json:"started,omitempty"`
	Fallback []string `json:"fallback"`
	Log      string   `json:"-"`
}

// LoadStatus reads the running proxy and the registry's fallback chain; the partial Status comes back with a load error.
func LoadStatus(ctx context.Context) (Status, error) {
	ps := Status{Log: LogFile()}
	if st, ok := Running(ctx); ok {
		ps.Running, ps.PID, ps.URL = true, st.PID, Base(st.Port)
		ps.Started = st.Started.Local().Format(time.RFC3339)
	}
	reg, err := providers.Load()
	if err != nil {
		return ps, err
	}
	ps.Fallback = append([]string{}, reg.Fallback...)
	return ps, nil
}
