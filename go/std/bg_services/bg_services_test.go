package bgservices

import (
	"context"
	"errors"
	"io"
	"log"
	"sync/atomic"
	"testing"
	"time"
)

// flakyService fails its first failures runs, then blocks until ctx ends.
type flakyService struct {
	failures int32
	runs     atomic.Int32
}

func (s *flakyService) Name() string { return "flaky" }

func (s *flakyService) Run(ctx context.Context) error {
	if s.runs.Add(1) <= s.failures {
		return errors.New("boom")
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *flakyService) Verify(ctx context.Context) error { return nil }

func TestSuperviseRestartsUntilHealthy(t *testing.T) {
	svc := &flakyService{failures: 3}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		supervise(ctx, svc, log.New(io.Discard, "", 0), time.Millisecond, 4*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for svc.runs.Load() < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("service was restarted %d times, want 4 runs", svc.runs.Load())
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervise did not return after cancel")
	}
	if got := svc.runs.Load(); got != 4 {
		t.Errorf("runs = %d, want 4 (3 failures + 1 healthy)", got)
	}
}

func TestSuperviseStopsImmediatelyOnCancel(t *testing.T) {
	svc := &flakyService{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		supervise(ctx, svc, log.New(io.Discard, "", 0), time.Millisecond, 4*time.Millisecond)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervise did not return after cancel")
	}
}
