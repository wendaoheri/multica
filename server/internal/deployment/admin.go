package deployment

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type AdminServer struct {
	addr       string
	store      *ControlStore
	controller *WorkerController
	generation int64
	owner      string
	token      string
	server     *http.Server
}

func NewAdminServer(addr string, store *ControlStore, controller *WorkerController, generation int64, owner, token string) (*AdminServer, error) {
	loopbackErr := ValidateLoopbackAddr(addr)
	if loopbackErr != nil && len(token) < 32 {
		return nil, fmt.Errorf("non-loopback worker admin requires a token of at least 32 bytes: %w", loopbackErr)
	}
	a := &AdminServer{addr: addr, store: store, controller: controller, generation: generation, owner: owner, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", a.status)
	mux.HandleFunc("POST /drain", a.drain)
	mux.HandleFunc("POST /complete-drain", a.completeDrain)
	mux.HandleFunc("POST /enable-claims", a.enableClaims)
	mux.HandleFunc("POST /activate", a.activate)
	mux.HandleFunc("POST /admission/open", a.openAdmission)
	mux.HandleFunc("POST /admission/close", a.closeAdmission)
	var handler http.Handler = mux
	if loopbackErr != nil || token != "" {
		handler = a.authenticate(handler)
	}
	a.server = &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	return a, nil
}

func (a *AdminServer) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(provided) != len(a.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(a.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *AdminServer) ListenAndServe() error { return a.server.ListenAndServe() }

func (a *AdminServer) Shutdown(ctx context.Context) error { return a.server.Shutdown(ctx) }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeControlError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, ErrGenerationMismatch) || errors.Is(err, ErrWorkerOwned) || errors.Is(err, ErrDrainIncomplete) {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (a *AdminServer) completeDrain(w http.ResponseWriter, r *http.Request) {
	if err := a.store.CompleteDrain(r.Context(), a.generation, a.owner, DrainObservationWindow); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "drained_owner_released"})
}

func (a *AdminServer) status(w http.ResponseWriter, _ *http.Request) {
	snapshot, err := a.store.Load(context.Background())
	if err != nil {
		writeControlError(w, err)
		return
	}
	value := map[string]any{"control": snapshot}
	if a.controller != nil {
		value["worker"] = a.controller.Status()
	}
	writeJSON(w, http.StatusOK, value)
}

func (a *AdminServer) drain(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Drain(r.Context(), a.generation, a.owner); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "draining"})
}

func (a *AdminServer) enableClaims(w http.ResponseWriter, r *http.Request) {
	if err := a.store.EnableClaims(r.Context(), a.generation, a.owner); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "claims_enabled"})
}

func (a *AdminServer) activate(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Activate(r.Context(), a.generation, a.owner); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"admission_open": true, "claims_enabled": true})
}

func (a *AdminServer) openAdmission(w http.ResponseWriter, r *http.Request) {
	a.setAdmission(w, r, true)
}

func (a *AdminServer) closeAdmission(w http.ResponseWriter, r *http.Request) {
	a.setAdmission(w, r, false)
}

func (a *AdminServer) setAdmission(w http.ResponseWriter, r *http.Request, open bool) {
	if err := a.store.SetAdmission(r.Context(), a.generation, open); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"admission_open": open})
}

func (a *AdminServer) String() string {
	return fmt.Sprintf("worker admin on %s generation=%d owner=%s", a.addr, a.generation, a.owner)
}
