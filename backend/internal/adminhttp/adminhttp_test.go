package adminhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-picpak/backend/internal/operator"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("bad json: %v (%s)", err, b)
	}
	return m
}

// T1: a missing or malformed bearer is 401 BEFORE any DB lookup (nil pool proves the lookup is not
// reached). Red: an admin route mounted without Auth answers 200.
func TestAuth_Rejects_401(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(599) }) // must NOT run
	h := Auth(nil)(next)
	for _, c := range []struct{ name, hdr string }{
		{"missing", ""},
		{"wrong-scheme", "Token abc"},
		{"empty-bearer", "Bearer "},
		{"only-spaces", "Bearer    "},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/whoami", nil)
			if c.hdr != "" {
				req.Header.Set("Authorization", c.hdr)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d", rec.Code)
			}
			if m := decode(t, rec.Body.Bytes()); m["success"] != false || m["code"] != "unauthorized" {
				t.Fatalf("want {success:false,code:unauthorized}, got %v", m)
			}
		})
	}
}

// T2: requireAdmin rejects a non-admin (and a missing operator — fail closed) with 403, and lets an
// admin through. Red: requireAdmin absent → a non-admin reaches the handler (200).
func TestRequireAdmin_403(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := RequireAdmin(ok)

	t.Run("non-admin", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req = req.WithContext(setOperator(req.Context(), operator.AuthResult{KeyID: 1, IsAdmin: false}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("want 403, got %d", rec.Code)
		}
		if m := decode(t, rec.Body.Bytes()); m["code"] != "forbidden" {
			t.Fatalf("want code forbidden, got %v", m)
		}
	})

	t.Run("no-operator-fail-closed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("want 403, got %d", rec.Code)
		}
	})

	t.Run("admin-passes", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req = req.WithContext(setOperator(req.Context(), operator.AuthResult{KeyID: 2, IsAdmin: true}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin: want 200, got %d", rec.Code)
		}
	})
}

// D17.5 envelope: success spreads fields at the top level; failure carries error+code.
func TestEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErr(rec, httptest.NewRequest("GET", "/x", nil), http.StatusTeapot, "teapot", "short and stout")
	m := decode(t, rec.Body.Bytes())
	if rec.Code != http.StatusTeapot || m["success"] != false || m["error"] != "short and stout" || m["code"] != "teapot" {
		t.Fatalf("err envelope wrong: %d %v", rec.Code, m)
	}

	rec = httptest.NewRecorder()
	WriteOK(rec, httptest.NewRequest("GET", "/x", nil), map[string]any{"key_id": float64(7), "is_admin": true})
	m = decode(t, rec.Body.Bytes())
	if m["success"] != true || m["key_id"] != float64(7) || m["is_admin"] != true {
		t.Fatalf("ok envelope wrong: %v", m)
	}
}

// HashToken is deterministic and 32 bytes (matches the operator_keys.token_hash CHECK).
func TestHashToken_Len(t *testing.T) {
	if h := operator.HashToken("hunter2"); len(h) != 32 {
		t.Fatalf("want 32-byte hash, got %d", len(h))
	}
}
