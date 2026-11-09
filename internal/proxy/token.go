package proxy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/ui/helptext"
)

var (
	tokenEnsure = Ensure
	tokenState  = Running
	tokenMint   = MintSessionToken
)

// Token ensures the daemon is running and mints a profile-scoped token bound to its PID.
func Token(ctx context.Context, profile string) (string, error) {
	if _, err := tokenEnsure(ctx); err != nil {
		return "", errors.New("proxy unavailable; run gateway proxy --start")
	}
	st, ok := tokenState(ctx)
	if !ok {
		return "", errors.New("proxy not running")
	}
	return tokenMint(ctx, profile, st.PID)
}

// Restart stops the proxy if running, starts it, and prints its base URL.
func Restart(ctx context.Context) int {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stoppedCode := 0
	if st, err := Stop(ctx); err == nil {
		stoppedCode = sayCode("proxy: stopped pid %d\n", st.PID)
	} else if !errors.Is(err, ErrNotRunning) {
		return helptext.Fail("proxy: %v", err)
	}
	base, err := Ensure(ctx)
	if err != nil {
		return helptext.Fail("proxy: %v", err)
	}
	if code := sayCode("%s\n", base); code != 0 {
		return code
	}
	return stoppedCode
}

// sayCode prints one stdout line; a failed write is an error exit, never a silent 0.
func sayCode(format string, a ...any) int {
	if _, err := fmt.Printf(format, a...); err != nil {
		return helptext.Fail("proxy: write output: %v", err)
	}
	return 0
}

// StopWithHold stops the proxy and holds off watchdog restarts until the next start.
func StopWithHold(ctx context.Context) int {
	if err := Hold(); err != nil {
		return helptext.Fail("proxy: hold: %v", err)
	}
	st, err := Stop(ctx)
	if errors.Is(err, ErrNotRunning) {
		return sayCode("proxy: not running\n")
	}
	if err != nil {
		return helptext.Fail("proxy: %v", err)
	}
	return sayCode("proxy: stopped pid %d\n", st.PID)
}
