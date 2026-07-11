package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-picpak/backend/internal/adminhttp"
)

// Q5 — read-only gate. All three log routes are auth-gated (any valid key), never RequireAdmin: a
// read-only operator must reach the viewer (D21.7). Red: a RequireAdmin wrap → a read-only operator is
// 403'd off the viewer; a missing Auth wrap → an unauthenticated request reaches the handler (200).
func TestLogRoutes_ReadOnlyGate_Q5(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "ro-tok", false) // a NON-admin operator key

	mux := http.NewServeMux()
	registerLogRoutes(mux, pool)
	h := adminhttp.WithRequestID(mux)

	code := func(path, token string) int {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		// Operator-key requests arrive over the loopback (SSH-tunnel) listener; tag the origin so the
		// W7 operator gate honours the bearer, mirroring production BaseContext.
		r = r.WithContext(adminhttp.WithListenerOrigin(r.Context(), adminhttp.OriginLoopback))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	for _, path := range []string{"/api/logs", "/api/logs/serials", "/api/logs/S/reconstruct?epoch=1"} {
		if c := code(path, "ro-tok"); c != http.StatusOK {
			t.Errorf("GET %s as read-only operator = %d, want 200 (read-only must not be admin-gated)", path, c)
		}
		if c := code(path, ""); c != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated = %d, want 401", path, c)
		}
	}
}
