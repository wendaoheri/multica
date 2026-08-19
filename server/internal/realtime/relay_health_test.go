package realtime

import (
	"errors"
	"testing"
	"time"
)

func TestRelayHealthConsumerFailureCannotBeMaskedByHeartbeat(t *testing.T) {
	h := NewRelayHealthTracker(1, time.Second, 2, 2)
	h.MarkStartupReady()
	h.MarkConsumer(0, 0, errors.New("xread failed"))
	h.MarkHeartbeat(nil)
	h.MarkConsumer(0, 0, errors.New("xread failed"))
	if err := h.Health(); err == nil {
		t.Fatal("consumer failure was masked by heartbeat")
	}
	h.MarkHeartbeat(nil)
	if err := h.Health(); err == nil {
		t.Fatal("write heartbeat restored failed consumer")
	}
	h.MarkConsumer(0, 0, nil)
	if err := h.Health(); err == nil {
		t.Fatal("single success bypassed recovery window")
	}
	h.MarkConsumer(0, 0, nil)
	if err := h.Health(); err != nil {
		t.Fatalf("health did not recover: %v", err)
	}
}

func TestRelayHealthFailsOnSustainedLag(t *testing.T) {
	h := NewRelayHealthTracker(1, time.Second, 2, 1)
	h.MarkStartupReady()
	h.MarkConsumer(0, 2*time.Second, nil)
	h.MarkConsumer(0, 2*time.Second, nil)
	if err := h.Health(); err == nil {
		t.Fatal("sustained lag remained healthy")
	}
}
