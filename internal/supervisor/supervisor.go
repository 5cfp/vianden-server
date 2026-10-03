// Package supervisor keeps long-running services alive.
//
// If a service returns an error or panics, it is restarted after a short wait.
// This way one failing service (for example voice, later) does not take the
// whole server down.
package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Backoff settings. Variables (not constants) so tests can make them shorter.
var (
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
	// healthyRunTime is how long a service must run before its backoff resets.
	healthyRunTime = time.Minute
)

// Run runs fn until ctx is cancelled.
//
//   - fn returns nil:           the service finished on purpose; Run returns.
//   - fn returns an error/panics: it is logged and fn is started again,
//     waiting longer after each failure in a row (1s, 2s, 4s ... up to 30s).
//
// fn must stop and return when its ctx is cancelled.
func Run(ctx context.Context, logger *slog.Logger, name string, fn func(context.Context) error) {
	backoff := initialBackoff
	for {
		started := time.Now()
		err := runSafely(ctx, fn)

		if ctx.Err() != nil {
			return // shutting down
		}
		if err == nil {
			logger.Info("service finished", "service", name)
			return
		}

		if time.Since(started) > healthyRunTime {
			backoff = initialBackoff // it ran fine for a while, so start over
		}
		logger.Error("service failed, restarting", "service", name, "error", err, "retry_in", backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// runSafely calls fn and turns a panic into an error.
func runSafely(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v\n%s", v, debug.Stack())
		}
	}()
	return fn(ctx)
}
