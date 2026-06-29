// Package adminhttp is the operator admin API's HTTP plumbing: the golden response envelope (D17.5),
// a per-request id, and the auth / requireAdmin middleware over operator keys (migration 0007).
//
// Auth is server-authoritative: every gate here is mirrored by a route that mounts it, so a missing
// wrapper is a visible 200 in the negatively-probed tests (T1/T2), not a silent hole.
package adminhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/operator"
)

type ctxKey int

const (
	opKey ctxKey = iota
	reqIDKey
)

// ---- response envelope (D17.5) ----

// WriteOK writes {"success":true, ...fields} with 200 and echoes X-Request-ID. The fields are spread
// at the top level (the golden shape A19's api.ts parses), not nested under a "data" key.
func WriteOK(w http.ResponseWriter, r *http.Request, fields map[string]any) {
	body := map[string]any{"success": true}
	for k, v := range fields {
		body[k] = v
	}
	writeJSON(w, r, http.StatusOK, body)
}

// WriteErr writes {"success":false,"error":msg,"code":code} with the given status.
func WriteErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, r, status, map[string]any{"success": false, "error": msg, "code": code})
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	if id := ReqID(r.Context()); id != "" {
		w.Header().Set("X-Request-ID", id)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ---- request id ----

// WithRequestID assigns a random request id, stored in the context and echoed on the response.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [8]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		ctx := context.WithValue(r.Context(), reqIDKey, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ReqID returns the request id, or "" if WithRequestID did not run.
func ReqID(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey).(string)
	return id
}

// ---- auth middleware ----

// Operator returns the authenticated operator from the context (set by Auth).
func Operator(ctx context.Context) (operator.AuthResult, bool) {
	op, ok := ctx.Value(opKey).(operator.AuthResult)
	return op, ok
}

// setOperator stores the operator in the context (used by Auth; exposed to tests in-package).
func setOperator(ctx context.Context, op operator.AuthResult) context.Context {
	return context.WithValue(ctx, opKey, op)
}

// Auth resolves the Bearer token to an operator (operator_keys, disabled_at IS NULL) and stores it in
// the context. A missing/garbage token or an unknown/revoked key returns 401. last_used_at is bumped
// async and never blocks the request.
func Auth(db *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearer(r)
			if !ok {
				WriteErr(w, r, http.StatusUnauthorized, "unauthorized", "missing or malformed bearer token")
				return
			}
			op, ok, err := operator.Authenticate(r.Context(), db, token)
			if err != nil {
				WriteErr(w, r, http.StatusInternalServerError, "internal", "auth lookup failed")
				return
			}
			if !ok {
				WriteErr(w, r, http.StatusUnauthorized, "unauthorized", "unknown or revoked key")
				return
			}
			go func() { _ = operator.TouchLastUsed(context.Background(), db, op.KeyID) }()
			next.ServeHTTP(w, r.WithContext(setOperator(r.Context(), op)))
		})
	}
}

// RequireAdmin rejects a non-admin operator with 403. Compose as Auth(db)(RequireAdmin(h)).
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op, ok := Operator(r.Context())
		if !ok || !op.IsAdmin {
			WriteErr(w, r, http.StatusForbidden, "forbidden", "admin privilege required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearer extracts the token from "Authorization: Bearer <t>". Returns ("", false) when the header is
// absent, the scheme is not Bearer, or the token is empty.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const pfx = "Bearer "
	if len(h) <= len(pfx) || !strings.EqualFold(h[:len(pfx)], pfx) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(pfx):])
	if tok == "" {
		return "", false
	}
	return tok, true
}
