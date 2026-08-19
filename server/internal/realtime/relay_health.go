package realtime

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	defaultRelayMaxConsumerLag = 30 * time.Second
	defaultRelayFailureLimit   = 3
	defaultRelayRecoveryLimit  = 2
)

type relayConsumerHealth struct {
	healthy   bool
	failures  int
	successes int
	lag       time.Duration
	lastError string
}

// RelayHealthTracker keeps publisher/heartbeat health separate from every
// consumer shard. A successful write heartbeat can never erase a consumer
// failure or sustained lag.
type RelayHealthTracker struct {
	mu            sync.RWMutex
	writeHealthy  bool
	heartbeatOK   bool
	consumers     []relayConsumerHealth
	maxLag        time.Duration
	failureLimit  int
	recoveryLimit int
}

func NewRelayHealthTracker(shards int, maxLag time.Duration, failureLimit, recoveryLimit int) *RelayHealthTracker {
	if maxLag <= 0 {
		maxLag = defaultRelayMaxConsumerLag
	}
	if failureLimit <= 0 {
		failureLimit = defaultRelayFailureLimit
	}
	if recoveryLimit <= 0 {
		recoveryLimit = defaultRelayRecoveryLimit
	}
	return &RelayHealthTracker{consumers: make([]relayConsumerHealth, shards), maxLag: maxLag, failureLimit: failureLimit, recoveryLimit: recoveryLimit}
}

func (h *RelayHealthTracker) MarkStartupReady() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writeHealthy, h.heartbeatOK = true, true
	for i := range h.consumers {
		h.consumers[i].healthy = true
	}
}

func (h *RelayHealthTracker) MarkWrite(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writeHealthy = err == nil
}

func (h *RelayHealthTracker) MarkHeartbeat(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.heartbeatOK = err == nil
}

func (h *RelayHealthTracker) MarkConsumer(shard int, lag time.Duration, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if shard < 0 || shard >= len(h.consumers) {
		return
	}
	c := &h.consumers[shard]
	c.lag = lag
	if err != nil || lag > h.maxLag {
		c.failures++
		c.successes = 0
		if err != nil {
			c.lastError = err.Error()
		} else {
			c.lastError = fmt.Sprintf("consumer lag %s exceeds %s", lag, h.maxLag)
		}
		if c.failures >= h.failureLimit {
			c.healthy = false
		}
		return
	}
	c.failures = 0
	c.successes++
	if c.successes >= h.recoveryLimit {
		c.healthy = true
		c.lastError = ""
	}
}

func (h *RelayHealthTracker) Health() error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.writeHealthy {
		return errors.New("relay publisher unhealthy")
	}
	if !h.heartbeatOK {
		return errors.New("relay heartbeat unhealthy")
	}
	for shard, c := range h.consumers {
		if !c.healthy {
			return fmt.Errorf("relay consumer shard %d unhealthy: %s", shard, c.lastError)
		}
	}
	return nil
}
