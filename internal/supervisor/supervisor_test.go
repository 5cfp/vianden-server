package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func init() {
	// Keep tests fast.
	initialBackoff = time.Millisecond
	maxBackoff = 5 * time.Millisecond
}

func TestRestartsAfterErrorAndPanic(t *testing.T) {
	calls := 0
	Run(context.Background(), discardLogger, "test", func(context.Context) error {
		calls++
		switch calls {
		case 1:
			return errors.New("boom")
		case 2:
			panic("kaboom")
		default:
			return nil // finished on purpose: Run should return
		}
	})

	if calls != 3 {
		t.Errorf("service ran %d times, want 3", calls)
	}
}

func TestStopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		Run(ctx, discardLogger, "test", func(ctx context.Context) error {
			<-ctx.Done() // a well-behaved service: run until asked to stop
			return ctx.Err()
		})
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}
