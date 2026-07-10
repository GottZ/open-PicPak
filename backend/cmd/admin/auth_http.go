package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminaudit"
	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/adminsession"
	"github.com/open-picpak/backend/internal/adminuser"
)

// sessionCookieName is the human-session cookie (design §4.1 / §4.3). It mirrors adminhttp's private
// carrier name; the login handler sets it, the middleware reads it.
const sessionCookieName = "ppk_sid"

type authHandlers struct{ pool *pgxpool.Pool }

// registerAuthRoutes mounts the human session-login surface (design §4.3). POST /api/session is the
// login itself (no auth — it IS the way in); DELETE /api/session is the authenticated logout. Single
// wiring source so main.go and the gating test can never drift (mirror of registerFaasRoutes).
func registerAuthRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := authHandlers{pool: pool}
	mux.Handle("POST /api/session", http.HandlerFunc(h.login))
	mux.Handle("DELETE /api/session", adminhttp.Auth(pool)(http.HandlerFunc(h.logout)))
}

// login — POST /api/session (no auth): verify username+password, open a session, set the ppk_sid
// cookie. Failure is UNIFORM — 401 "invalid credentials" with the SAME body AND the SAME latency for a
// wrong password and an unknown user (adminuser.Verify runs a dummy argon2 for an unknown username), so
// nothing leaks which usernames exist (design §4.3 / §5). The verify runs under the global argon2
// semaphore (shared with the Basic carrier); a saturated semaphore is a 429, never an unbounded pile of
// ~64 MiB verifies. Both outcomes write admin_audit (session.login / session.login_fail) with the
// non-spoofable remote_ip — the failure synchronously, so a crash cannot swallow it (design §4.6).
func (h authHandlers) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	remoteIP := adminhttp.RemoteIP(r)

	if !adminhttp.TryAcquireArgon2() {
		w.Header().Set("Retry-After", "1")
		adminhttp.WriteErr(w, r, http.StatusTooManyRequests, "rate_limited", "authentication capacity exceeded; retry shortly")
		return
	}
	u, ok, err := adminuser.Verify(r.Context(), h.pool, body.Username, body.Password)
	adminhttp.ReleaseArgon2() // free the ~64 MiB slot the moment the verify returns, before the DB write
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "login failed")
		return
	}
	if !ok {
		_ = adminaudit.Write(r.Context(), h.pool, adminaudit.Entry{
			ActorKind: "session", Action: "session.login_fail", Target: body.Username, RemoteIP: remoteIP, OK: false,
		})
		adminhttp.WriteErr(w, r, http.StatusUnauthorized, "unauthorized", "invalid credentials")
		return
	}

	ttl := sessionTTL()
	secret, sess, err := adminsession.Create(r.Context(), h.pool, u.ID, ttl)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "session create failed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    secret,
		Path:     "/",
		HttpOnly: true,                    // no JS access (XSS-exfil hardening, design §5 B7)
		Secure:   true,                    // TLS only — the reverse proxy terminates TLS
		SameSite: http.SameSiteStrictMode, // + X-Requested-With on mutations = CSRF defence (§4.1)
		MaxAge:   int(ttl / time.Second),
	})
	_ = adminaudit.Write(r.Context(), h.pool, adminaudit.Entry{
		ActorKind: "session", ActorID: strconv.FormatInt(u.ID, 10), Action: "session.login",
		Target: u.Username, RemoteIP: remoteIP, OK: true,
	})
	adminhttp.WriteOK(w, r, map[string]any{
		"user_id": u.ID, "username": u.Username, "is_admin": u.IsAdmin, "expires_at": sess.ExpiresAt,
	})
}

// logout — DELETE /api/session (session auth): delete the session row (server-authoritative revoke) and
// clear the cookie with Max-Age=0.
func (h authHandlers) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		if err := adminsession.Delete(r.Context(), h.pool, c.Value); err != nil {
			adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "logout failed")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1, // Go emits Max-Age=0, clearing the cookie
	})
	adminhttp.WriteOK(w, r, map[string]any{"logged_out": true})
}

// sessionTTL is the absolute, server-authoritative session lifetime (Policy=Data).
func sessionTTL() time.Duration { return envDurOr("ADMIN_SESSION_TTL", 24*time.Hour) }

// startSessionPrune purges expired admin_sessions on a ticker until ctx is cancelled (design §3.2) —
// an absolute expires_at makes an expired row dead weight, and without this sweep the table grows
// monotonically with every login. Mirror of startFragmentPrune.
func startSessionPrune(ctx context.Context, pool *pgxpool.Pool) {
	interval := envDurOr("ADMIN_SESSION_PRUNE_INTERVAL", time.Hour)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				switch n, err := adminsession.Cleanup(ctx, pool); {
				case err != nil:
					log.Printf("admin session prune: %v", err)
				case n > 0:
					log.Printf("admin session prune removed %d rows", n)
				}
			}
		}
	}()
}
