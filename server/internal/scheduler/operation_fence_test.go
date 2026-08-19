package scheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestHeartbeatRenewChecksOperationFenceBeforeDatabase(t *testing.T) {
	checked := make(chan struct{}, 1)
	m := NewManager(nil, Options{OperationGuard: func(context.Context, string, bool, func(context.Context) error) error {
		select {
		case checked <- struct{}{}:
		default:
		}
		return context.Canceled
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go m.runHeartbeats(ctx, done, &JobSpec{HeartbeatInterval: time.Millisecond, StaleTimeout: time.Second}, claim{}, slog.Default())
	select {
	case <-checked:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not check fence")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat loop did not stop")
	}
}
