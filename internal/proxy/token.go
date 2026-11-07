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
	if st, err := Stop(ctx); err == nil {
		fmt.Printf("proxy: stopped pid %d\n", st.PID)
	} else if !errors.Is(err, ErrNotRunning) {
		return helptext.Fail("proxy: %v", err)
	}
	base, err := Ensure(ctx)
	if err != nil {
		return helptext.Fail("proxy: %v", err)
	}
	fmt.Println(base)
	return 0
}

// StopWithHold stops the proxy and holds off watchdog restarts until the next start.
func StopWithHold(ctx context.Context) int {
	if err := Hold(); err != nil {
		return helptext.Fail("proxy: hold: %v", err)
	}
	st, err := Stop(ctx)
	if errors.Is(err, ErrNotRunning) {
		fmt.Println("proxy: not running")
		return 0
	}
	if err != nil {
		return helptext.Fail("proxy: %v", err)
	}
	fmt.Printf("proxy: stopped pid %d\n", st.PID)
	return 0
}
