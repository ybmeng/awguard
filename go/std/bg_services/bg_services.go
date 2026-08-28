// Package bgservices defines the contract every std background service
// implements, plus the supervisor loop stdd uses to keep them running.
//
// A background service has one job, uses only the standard library, and must
// be quickly verifiable: Verify is a fast self-check proving the service can
// do its job right now.
package bgservices

import (
	"context"
	"log"
	"time"
)

// Service is one std background service. Implementations must be safe to
// restart: Run may be called again after it returns an error.
type Service interface {
	// Name is a short, stable identifier used in logs and verify output.
	Name() string
	// Run does the service's work until ctx is canceled.
	Run(ctx context.Context) error
	// Verify performs a fast self-check (well under a second) and returns
	// nil if the service is currently able to do its job.
	Verify(ctx context.Context) error
}

const (
	restartBackoffMin = time.Second
	restartBackoffMax = time.Minute
)

// Supervise runs svc until ctx is canceled, restarting it with exponential
// backoff whenever Run returns early. It blocks until ctx ends.
func Supervise(ctx context.Context, svc Service, logger *log.Logger) {
	supervise(ctx, svc, logger, restartBackoffMin, restartBackoffMax)
}

func supervise(ctx context.Context, svc Service, logger *log.Logger, backoffMin, backoffMax time.Duration) {
	if logger == nil {
		logger = log.Default()
	}
	backoff := backoffMin
	for {
		started := time.Now()
		err := svc.Run(ctx)
		if ctx.Err() != nil {
			logger.Printf("stdd: %s stopped", svc.Name())
			return
		}
		if time.Since(started) > backoffMax {
			// Ran long enough to be considered healthy; reset the backoff.
			backoff = backoffMin
		}
		logger.Printf("stdd: %s exited (%v), restarting in %s", svc.Name(), err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > backoffMax {
			backoff = backoffMax
		}
	}
}
