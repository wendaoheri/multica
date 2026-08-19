package deployment

import (
	"context"
	"sync"
	"time"
)

type WorkerState string

const (
	WorkerClaimsDisabled WorkerState = "claims_disabled"
	WorkerRunning        WorkerState = "running"
	WorkerDraining       WorkerState = "draining"
	WorkerDrained        WorkerState = "drained"
	WorkerFenced         WorkerState = "fenced"
	WorkerRelayUnhealthy WorkerState = "relay_unhealthy"
)

type WorkerStatus struct {
	State              WorkerState `json:"state"`
	ExpectedGeneration int64       `json:"expected_generation"`
	ActiveGeneration   int64       `json:"active_generation"`
	AcceptingNew       bool        `json:"accepting_new"`
	Owner              string      `json:"owner"`
	LastError          string      `json:"last_error,omitempty"`
}

type WorkerController struct {
	store      workerControlStore
	generation int64
	owner      string
	relayOK    func(context.Context) error
	poll       time.Duration

	mu     sync.RWMutex
	status WorkerStatus
}

type workerControlStore interface {
	Load(context.Context) (ControlSnapshot, error)
	Heartbeat(context.Context, int64, string) error
}

func NewWorkerController(store workerControlStore, generation int64, owner string, relayOK func(context.Context) error) *WorkerController {
	if relayOK == nil {
		relayOK = func(context.Context) error { return nil }
	}
	return &WorkerController{
		store: store, generation: generation, owner: owner, relayOK: relayOK,
		poll:   time.Second,
		status: WorkerStatus{State: WorkerClaimsDisabled, ExpectedGeneration: generation, Owner: owner},
	}
}

func (c *WorkerController) Status() WorkerStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

func (c *WorkerController) update(fn func(*WorkerStatus)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(&c.status)
}

// Run waits in claims-disabled state, starts the supplied worker composition
// exactly once after the DB fence binds this owner, and cancels it immediately
// on drain, generation change, relay failure, or control-plane read failure.
func (c *WorkerController) Run(ctx context.Context, run func(context.Context) error) error {
	ticker := time.NewTicker(c.poll)
	defer ticker.Stop()
	for {
		if err := c.relayOK(ctx); err != nil {
			c.update(func(s *WorkerStatus) { s.State = WorkerRelayUnhealthy; s.LastError = err.Error() })
			return err
		}
		snapshot, err := c.store.Load(ctx)
		if err != nil {
			c.update(func(s *WorkerStatus) { s.State = WorkerFenced; s.LastError = err.Error() })
			return err
		}
		c.update(func(s *WorkerStatus) { s.ActiveGeneration = snapshot.ActiveGeneration })
		if snapshot.ActiveGeneration != c.generation {
			c.update(func(s *WorkerStatus) { s.State = WorkerFenced })
			return ErrGenerationMismatch
		}
		if snapshot.ClaimsEnabled && snapshot.WorkerOwner == c.owner {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}

	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	c.update(func(s *WorkerStatus) { s.State = WorkerRunning; s.AcceptingNew = true; s.LastError = "" })
	go func() { done <- run(workerCtx) }()

	for {
		select {
		case err := <-done:
			c.update(func(s *WorkerStatus) { s.State = WorkerDrained; s.AcceptingNew = false })
			return err
		case <-ctx.Done():
			c.update(func(s *WorkerStatus) { s.State = WorkerDraining; s.AcceptingNew = false })
			cancel()
			return <-done
		case <-ticker.C:
		}

		if err := c.relayOK(ctx); err != nil {
			c.update(func(s *WorkerStatus) {
				s.State = WorkerRelayUnhealthy
				s.AcceptingNew = false
				s.LastError = err.Error()
			})
			cancel()
			<-done
			return err
		}
		if err := c.store.Heartbeat(ctx, c.generation, c.owner); err != nil {
			c.update(func(s *WorkerStatus) { s.State = WorkerDraining; s.AcceptingNew = false; s.LastError = err.Error() })
			cancel()
			<-done
			if snapshot, loadErr := c.store.Load(context.Background()); loadErr == nil && snapshot.ActiveGeneration != c.generation {
				c.update(func(s *WorkerStatus) { s.State = WorkerFenced })
			}
			return err
		}
	}
}
