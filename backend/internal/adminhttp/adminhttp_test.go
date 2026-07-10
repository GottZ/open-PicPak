package adminhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-picpak/backend/internal/operator"
)

// setPrincipalCtx is the test-side seam for injecting a resolved Principal (Auth's job at runtime).
func setPrincipalCtx(r *http.Request, p Principal) *http.Request {
	return r.WithContext(setPrincipal(r.Context(), p))
}

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

// T2: requireAdmin rejects a non-admin Principal (and a missing one — fail closed) with 403, and
// lets an admin through. Red: requireAdmin absent → a non-admin reaches the handler (200).
func TestRequireAdmin_403(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := RequireAdmin(ok)

	t.Run("non-admin", func(t *testing.T) {
		req := setPrincipalCtx(httptest.NewRequest("GET", "/x", nil), Principal{Kind: KindBearer, ID: "t1", Scopes: implicitFullScopes()})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("want 403, got %d", rec.Code)
		}
		if m := decode(t, rec.Body.Bytes()); m["code"] != "forbidden" {
			t.Fatalf("want code forbidden, got %v", m)
		}
	})

	t.Run("no-principal-fail-closed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("want 403, got %d", rec.Code)
		}
	})

	t.Run("admin-passes", func(t *testing.T) {
		req := setPrincipalCtx(httptest.NewRequest("GET", "/x", nil), Principal{Kind: KindSession, ID: "2", IsAdmin: true})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin: want 200, got %d", rec.Code)
		}
	})
}

// T3: RequireScope("image:write") lets a Principal carrying the scope through and rejects one that
// lacks it (image:read only) with 403 — probe (b). A missing Principal fails closed. Red: no
// RequireScope wrapper → an image:read token reaches a write handler (200).
func TestRequireScope_403(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := RequireScope(ScopeImageWrite)(ok)

	t.Run("read-only-token-on-write-route", func(t *testing.T) {
		req := setPrincipalCtx(httptest.NewRequest("POST", "/api/images", nil),
			Principal{Kind: KindBearer, ID: "ro", Scopes: []string{ScopeImageRead}})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("read-only on write route: want 403, got %d", rec.Code)
		}
		if m := decode(t, rec.Body.Bytes()); m["code"] != "forbidden" {
			t.Fatalf("want code forbidden, got %v", m)
		}
	})

	t.Run("write-token-passes", func(t *testing.T) {
		req := setPrincipalCtx(httptest.NewRequest("POST", "/api/images", nil),
			Principal{Kind: KindBearer, ID: "rw", Scopes: []string{ScopeImageRead, ScopeImageWrite}})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("write token: want 200, got %d", rec.Code)
		}
	})

	t.Run("session-implicitly-full-passes", func(t *testing.T) {
		req := setPrincipalCtx(httptest.NewRequest("POST", "/api/images", nil),
			Principal{Kind: KindSession, ID: "1", Scopes: implicitFullScopes(), IsAdmin: true})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("session: want 200, got %d", rec.Code)
		}
	})

	t.Run("no-principal-fail-closed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/images", nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("no principal: want 403, got %d", rec.Code)
		}
	})
}

// TestPrincipal_HasScope: an api_token carries exactly its scopes; a session/operator holds the full
// image set. An api_token is never admin (RequireAdmin unreachable, design §5 B3).
func TestPrincipal_HasScope(t *testing.T) {
	ro := Principal{Kind: KindBearer, Scopes: []string{ScopeImageRead}}
	if !ro.HasScope(ScopeImageRead) || ro.HasScope(ScopeImageWrite) || ro.IsAdmin {
		t.Fatalf("read-only bearer scope/admin wrong: %+v", ro)
	}
	sess := Principal{Kind: KindSession, Scopes: implicitFullScopes()}
	if !sess.HasScope(ScopeImageRead) || !sess.HasScope(ScopeImageWrite) {
		t.Fatalf("session should hold both image scopes: %+v", sess)
	}
}

// TestListenerOrigin_Tag: the origin round-trips through the context and is absent when unset. This
// is the W7-prep tag only (no policy in W2).
func TestListenerOrigin_Tag(t *testing.T) {
	if _, ok := ListenerOriginFrom(context.Background()); ok {
		t.Fatal("untagged context must report no origin")
	}
	ctx := WithListenerOrigin(context.Background(), OriginLoopback)
	if o, ok := ListenerOriginFrom(ctx); !ok || o != OriginLoopback {
		t.Fatalf("want loopback, got %v ok=%v", o, ok)
	}
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
