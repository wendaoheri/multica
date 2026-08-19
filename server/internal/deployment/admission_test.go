package deployment

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdmissionExempt(t *testing.T) {
	for _, tt := range []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodGet, "/api/issues", true},
		{http.MethodPost, "/healthz", true},
		{http.MethodPost, "/auth/verify-code", true},
		{http.MethodPost, "/api/issues", false},
		{http.MethodDelete, "/api/issues/1", false},
	} {
		r := httptest.NewRequest(tt.method, tt.path, nil)
		if got := admissionExempt(r); got != tt.want {
			t.Fatalf("admissionExempt(%s %s) = %v, want %v", tt.method, tt.path, got, tt.want)
		}
	}
}
