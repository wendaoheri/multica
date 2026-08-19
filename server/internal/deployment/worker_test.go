package deployment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeWorkerStore struct {
	mu       sync.Mutex
	snapshot ControlSnapshot
	heartErr error
}

func (f *fakeWorkerStore) Load(context.Context) (ControlSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot, nil
}

func (f *fakeWorkerStore) Heartbeat(context.Context, int64, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartErr
}

func TestWorkerControllerStartsOnlyForBoundOwnerAndDrainsOnFence(t *testing.T) {
	store := &fakeWorkerStore{snapshot: ControlSnapshot{ActiveGeneration: 7}}
	c := NewWorkerController(store, 7, "worker-a", nil)
	c.poll = time.Millisecond
	started := make(chan struct{})
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- c.Run(ctx, func(runCtx context.Context) error {
			close(started)
			<-runCtx.Done()
			return nil
		})
	}()

	select {
	case <-started:
		t.Fatal("worker started while claims were disabled")
	case <-time.After(10 * time.Millisecond):
	}
	store.mu.Lock()
	store.snapshot.ClaimsEnabled = true
	store.snapshot.WorkerOwner = "worker-a"
	store.mu.Unlock()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start after owner binding")
	}
	store.mu.Lock()
	store.heartErr = ErrGenerationMismatch
	store.snapshot.ActiveGeneration = 8
	store.mu.Unlock()
	if err := <-done; !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("Run returned %v, want generation mismatch", err)
	}
	if got := c.Status(); got.State != WorkerFenced || got.AcceptingNew {
		t.Fatalf("unexpected status after fence: %+v", got)
	}
}

func TestWorkerControllerFailsClosedOnRelayError(t *testing.T) {
	store := &fakeWorkerStore{snapshot: ControlSnapshot{
		ActiveGeneration: 3,
		ClaimsEnabled:    true,
		WorkerOwner:      "worker-b",
	}}
	relayErr := errors.New("redis unavailable")
	c := NewWorkerController(store, 3, "worker-b", func(context.Context) error { return relayErr })
	called := false
	err := c.Run(context.Background(), func(context.Context) error { called = true; return nil })
	if !errors.Is(err, relayErr) || called {
		t.Fatalf("Run = %v, called=%v; want relay failure before start", err, called)
	}
	if got := c.Status(); got.State != WorkerRelayUnhealthy || got.AcceptingNew {
		t.Fatalf("unexpected relay failure status: %+v", got)
	}
}
