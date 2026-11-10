package gateway

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

// LoadStatus reads the running proxy and the registry's fallback chain; a load error returns the partial Status.
func LoadStatus(ctx context.Context) (Status, error) {
	ps := Status{Log: logFile()}
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
