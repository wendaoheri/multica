package deployment

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AdmissionGate caches the durable release gate briefly. Any database or
// relay-health uncertainty denies mutating API requests in split mode.
type AdmissionGate struct {
	store   *ControlStore
	relayOK func(context.Context) error
	ttl     time.Duration

	mu        sync.Mutex
	checkedAt time.Time
	open      bool
}

func NewAdmissionGate(store *ControlStore, relayOK func(context.Context) error) *AdmissionGate {
	return &AdmissionGate{store: store, relayOK: relayOK, ttl: time.Second}
}

func (g *AdmissionGate) IsOpen(ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.checkedAt) < g.ttl {
		return g.open
	}
	g.checkedAt = time.Now()
	g.open = false
	if g.relayOK == nil || g.relayOK(ctx) != nil {
		return false
	}
	snapshot, err := g.store.Load(ctx)
	if err == nil {
		g.open = snapshot.AdmissionOpen
	}
	return g.open
}

func (g *AdmissionGate) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if admissionExempt(r) || g.IsOpen(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":       "release admission is closed",
			"reason_code": "release_admission_closed",
		})
	})
}

// RelayReadinessMiddleware makes split Web readiness include the transport it
// needs for cross-process daemon wakeups and realtime fanout. Liveness remains
// independent so an operator can distinguish an alive-but-fenced process.
func RelayReadinessMiddleware(relayOK func(context.Context) error, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.URL.Path == "/readyz" || r.URL.Path == "/healthz") && relayOK(r.Context()) != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "not_ready",
				"reason": "realtime_relay_unhealthy",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func admissionExempt(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	for _, prefix := range []string{"/auth/", "/health", "/readyz", "/healthz"} {
		if strings.HasPrefix(r.URL.Path, prefix) {
			return true
		}
	}
	return false
}
