package deployment

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeWorkerStore struct {
	mu           sync.Mutex
	snapshot     ControlSnapshot
	heartErr     error
	drainBlock   <-chan struct{}
	drainStarted chan struct{}
	drainErr     error
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

func (f *fakeWorkerStore) Drain(ctx context.Context, generation int64, owner string) error {
	if f.drainStarted != nil {
		select {
		case f.drainStarted <- struct{}{}:
		default:
		}
	}
	if f.drainBlock != nil {
		select {
		case <-f.drainBlock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.drainErr != nil {
		return f.drainErr
	}
	if f.snapshot.ActiveGeneration != generation || f.snapshot.WorkerOwner != owner {
		return ErrGenerationMismatch
	}
	f.snapshot.ClaimsEnabled = false
	f.snapshot.AdmissionOpen = false
	f.snapshot.DrainRequested = true
	return nil
}

func TestRelayFailureCancelsLocallyBeforeBoundedDrainPersistence(t *testing.T) {
	block := make(chan struct{})
	drainStarted := make(chan struct{}, 1)
	store := &fakeWorkerStore{snapshot: ControlSnapshot{ActiveGeneration: 12, ClaimsEnabled: true, AdmissionOpen: true, WorkerOwner: "worker-a"}, drainBlock: block, drainStarted: drainStarted}
	var fail sync.Mutex
	unhealthy := false
	c := NewWorkerController(store, 12, "worker-a", func(context.Context) error {
		fail.Lock()
		defer fail.Unlock()
		if unhealthy {
			return errors.New("relay consume failed")
		}
		return nil
	})
	c.poll = time.Millisecond
	c.drainTimeout = 20 * time.Millisecond
	started := make(chan struct{})
	cancelled := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.Run(context.Background(), func(ctx context.Context) error { close(started); <-ctx.Done(); close(cancelled); return ctx.Err() })
	}()
	<-started
	fail.Lock()
	unhealthy = true
	fail.Unlock()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("composition was not cancelled")
	}
	select {
	case <-drainStarted:
	case <-time.After(time.Second):
		t.Fatal("bounded drain was not attempted after cancel")
	}
	if err := c.RunFenced(context.Background(), "new-claim", true, func(context.Context) error { t.Fatal("operation ran after local fail-close"); return nil }); err == nil {
		t.Fatal("new operation accepted during blocked DB drain")
	}
	if err := <-done; err == nil {
		t.Fatal("relay failure hidden")
	}
	if !strings.Contains(c.Status().LastError, "deadline exceeded") {
		t.Fatalf("drain timeout not observable: %+v", c.Status())
	}
}

func (f *fakeWorkerStore) acquire(generation int64, owner string, lease bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshot.ActiveGeneration != generation || f.snapshot.WorkerOwner != owner || !f.snapshot.ClaimsEnabled || f.snapshot.DrainRequested {
		return ErrGenerationMismatch
	}
	if lease {
		f.snapshot.WorkerLeases++
	} else {
		f.snapshot.WorkerInFlight++
	}
	return nil
}
func (f *fakeWorkerStore) release(generation int64, owner string, lease bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshot.ActiveGeneration != generation || f.snapshot.WorkerOwner != owner {
		return ErrGenerationMismatch
	}
	if lease {
		f.snapshot.WorkerLeases--
	} else {
		f.snapshot.WorkerInFlight--
	}
	return nil
}
func (f *fakeWorkerStore) AcquireOperation(_ context.Context, generation int64, owner string) error {
	return f.acquire(generation, owner, false)
}
func (f *fakeWorkerStore) ReleaseOperation(_ context.Context, generation int64, owner string) error {
	return f.release(generation, owner, false)
}
func (f *fakeWorkerStore) AcquireLease(_ context.Context, generation int64, owner string) error {
	return f.acquire(generation, owner, true)
}
func (f *fakeWorkerStore) ReleaseLease(_ context.Context, generation int64, owner string) error {
	return f.release(generation, owner, true)
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

func TestWorkerControllerRuntimeRelayFailureDrainsClaims(t *testing.T) {
	store := &fakeWorkerStore{snapshot: ControlSnapshot{
		ActiveGeneration: 9,
		ClaimsEnabled:    true,
		AdmissionOpen:    true,
		WorkerOwner:      "worker-a",
	}}
	var relayMu sync.Mutex
	var relayErr error
	c := NewWorkerController(store, 9, "worker-a", func(context.Context) error {
		relayMu.Lock()
		defer relayMu.Unlock()
		return relayErr
	})
	c.poll = time.Millisecond
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.Run(context.Background(), func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	<-started
	wantErr := errors.New("consumer stalled")
	relayMu.Lock()
	relayErr = wantErr
	relayMu.Unlock()
	if err := <-done; !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want relay error", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.snapshot.ClaimsEnabled || store.snapshot.AdmissionOpen || !store.snapshot.DrainRequested {
		t.Fatalf("relay failure did not drain control: %+v", store.snapshot)
	}
	if got := c.Status(); got.State != WorkerRelayUnhealthy || got.AcceptingNew {
		t.Fatalf("relay failure status = %+v", got)
	}
}

func TestRunFencedRejectsOldGenerationAndTracksLease(t *testing.T) {
	store := &fakeWorkerStore{snapshot: ControlSnapshot{ActiveGeneration: 4, ClaimsEnabled: true, WorkerOwner: "worker-a"}}
	c := NewWorkerController(store, 4, "worker-a", nil)
	if err := c.RunFenced(context.Background(), "scheduler", true, func(context.Context) error {
		store.mu.Lock()
		defer store.mu.Unlock()
		if store.snapshot.WorkerLeases != 1 {
			t.Fatalf("leases = %d, want 1", store.snapshot.WorkerLeases)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.snapshot.ActiveGeneration = 5
	store.mu.Unlock()
	called := false
	if err := c.RunFenced(context.Background(), "scheduler", true, func(context.Context) error { called = true; return nil }); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("old generation RunFenced error = %v", err)
	}
	if called {
		t.Fatal("old generation operation ran")
	}
}
