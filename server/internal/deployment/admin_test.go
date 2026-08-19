package deployment

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminServerRejectsUnauthenticatedRemoteBind(t *testing.T) {
	if _, err := NewAdminServer("0.0.0.0:9091", nil, nil, 1, "worker", "short"); err == nil {
		t.Fatal("expected non-loopback bind with short token to fail")
	}
}

func TestAdminServerRemoteBindRequiresBearerToken(t *testing.T) {
	token := strings.Repeat("a", 32)
	server, err := NewAdminServer("0.0.0.0:9091", nil, nil, 1, "worker", token)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
