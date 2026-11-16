package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/textutil"
)

func proxyStatus(ctx context.Context) (detail string, bad bool) {
	st, err := gateway.ReadState()
	if errors.Is(err, os.ErrNotExist) {
		return "not running", false
	}
	if err != nil || st.Port == 0 {
		return "proxy.json unreadable (cannot check the running build)", true
	}
	h, ok := gateway.Holder(ctx, st.Port)
	if !ok {
		return "stale proxy.json (port " + strconv.Itoa(st.Port) + " not answering)", false
	}
	if h.Build != textutil.BuildVersion() {
		return fmt.Sprintf("pid %d runs build %s; restart when idle", h.PID, h.Build), true
	}
	return fmt.Sprintf("port %d pid %d, %d sessions", st.Port, h.PID, h.Sessions), false
}
