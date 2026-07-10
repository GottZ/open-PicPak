// Package adminhttp is the operator admin API's HTTP plumbing: the golden response envelope (D17.5),
// a per-request id, and the auth middleware that resolves a request to a single Principal.
//
// Auth is server-authoritative: every gate here is mirrored by a route that mounts it, so a missing
// wrapper is a visible 200 in the negatively-probed tests (T1/T2), not a silent hole. The middleware
// normalises four carriers onto ONE Principal (design §4.1 / delta-4), dispatched in this order:
//
//   - Authorization: Bearer ppk_<id>_<secret>  → an api_tokens machine identity (scoped, never admin)
//   - Authorization: Bearer <operator-token>   → a legacy operator_keys identity (fallback; W7 gates
//     this to the loopback listener, W2 keeps it as an ungated fallback)
//   - Authorization: Basic <b64>               → an admin_users identity (delta-4), verified identically
//     to POST /api/session; NO WWW-Authenticate is ever set, keeping Basic a non-browser scheme
//   - Cookie: ppk_sid=<secret>                 → a human admin_sessions identity (implicitly full);
//     mutations additionally require X-Requested-With: picpak (CSRF second layer, design §4.1)
//
// Any missing / malformed / unknown / wrong / expired / revoked credential resolves to a uniform 401
// with the same body — no enumeration signal (design §5).
package adminhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminsession"
	"github.com/open-picpak/backend/internal/adminuser"
	"github.com/open-picpak/backend/internal/apitoken"
	"github.com/open-picpak/backend/internal/operator"
)

type ctxKey int

const (
	opKey ctxKey = iota
	reqIDKey
	principalKey
	originKey
)

// sessionCookie is the human-session carrier's cookie name (design §4.1 / §4.3).
const sessionCookie = "ppk_sid"

// The CSRF header every MUTATING cookie-authed request must carry (design §4.1): the session cookie
// is an ambient credential the browser attaches automatically, so SameSite=Strict alone is one layer —
// this header is the second. The SPA sets it on every mutation (api.ts); a cross-site <form> POST
// cannot set a custom header. Only the cookie carrier enforces it: Bearer/Basic are not ambient and
// stay CSRF-immune.
const (
	csrfHeader      = "X-Requested-With"
	csrfHeaderValue = "picpak"
)

// csrfSafeMethod reports whether a method is read-only for CSRF purposes (no state change → no gate).
func csrfSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// image scopes an api_token can carry; a session or operator Principal holds both implicitly.
const (
	ScopeImageRead  = "image:read"
	ScopeImageWrite = "image:write"
)

// ---- Principal (the one normalised identity, design §4.1) ----

// PrincipalKind names the carrier that produced a Principal.
type PrincipalKind string

const (
	KindOperator PrincipalKind = "operator" // legacy operator_keys bearer
	KindBearer   PrincipalKind = "bearer"   // api_tokens machine bearer (ppk_)
	KindSession  PrincipalKind = "session"  // admin_sessions human cookie
	KindBasic    PrincipalKind = "basic"    // admin_users HTTP Basic identity (delta-4)
)

// Principal is the single identity every carrier resolves to. Scopes is the effective set: an
// api_token carries exactly its granted scopes, while a session or operator identity is seeded with
// the full image scope set (a human / full operator may manage images). IsAdmin is the fleet-admin
// flag (RequireAdmin) — structurally false for an api_token, so a leaked image token can never reach
// the RCE-capable routes (design §4.1 / §5 B3).
type Principal struct {
	Kind    PrincipalKind `json:"kind"`
	ID      string        `json:"id"`
	Label   string        `json:"label"`
	Scopes  []string      `json:"scopes"`
	IsAdmin bool          `json:"is_admin"`
}

// HasScope reports whether the Principal carries the given scope.
func (p Principal) HasScope(scope string) bool { return slices.Contains(p.Scopes, scope) }

// implicitFullScopes is the image scope set a session / operator Principal holds. A fresh slice per
// call so a handler mutating it cannot corrupt another request.
func implicitFullScopes() []string { return []string{ScopeImageRead, ScopeImageWrite} }

func setPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFrom returns the authenticated Principal from the context (set by Auth).
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

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

// unauthorized writes the ONE 401 every failed carrier shares — same status, code and body, so a
// wrong token is indistinguishable from an unknown or absent one (design §5, no enumeration signal).
func unauthorized(w http.ResponseWriter, r *http.Request) {
	WriteErr(w, r, http.StatusUnauthorized, "unauthorized", "invalid or missing credentials")
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

// ---- listener origin (design §4.1, W7 prep — the tag only, no policy yet) ----

// ListenerOrigin marks which socket a request arrived on. The value comes from the bind address, not
// from any client-settable header, so it is not spoofable. W2 only SETS it (ListenerBaseContext);
// W7 reads it to gate the operator_key bearer fallback to the loopback listener.
type ListenerOrigin string

const (
	OriginLoopback ListenerOrigin = "loopback"
	OriginPublic   ListenerOrigin = "public"
)

// WithListenerOrigin tags a context with its listener origin.
func WithListenerOrigin(ctx context.Context, o ListenerOrigin) context.Context {
	return context.WithValue(ctx, originKey, o)
}

// ListenerOriginFrom returns the request's listener origin, or (_, false) if untagged.
func ListenerOriginFrom(ctx context.Context) (ListenerOrigin, bool) {
	o, ok := ctx.Value(originKey).(ListenerOrigin)
	return o, ok
}

// ListenerBaseContext returns an http.Server.BaseContext hook that tags every request on that
// listener with the given origin. Wire one http.Server per listener so each carries its own origin.
func ListenerBaseContext(parent context.Context, o ListenerOrigin) func(net.Listener) context.Context {
	return func(net.Listener) context.Context { return WithListenerOrigin(parent, o) }
}

// ---- auth middleware ----

// Operator returns the authenticated operator from the context. It is populated only on the
// operator-key carrier (legacy bearer); api-token and session Principals have none. Retained for the
// existing handlers that read the operator identity directly (whoami, command attribution).
func Operator(ctx context.Context) (operator.AuthResult, bool) {
	op, ok := ctx.Value(opKey).(operator.AuthResult)
	return op, ok
}

// setOperator stores the operator in the context (used by Auth; exposed to tests in-package).
func setOperator(ctx context.Context, op operator.AuthResult) context.Context {
	return context.WithValue(ctx, opKey, op)
}

// Auth resolves the request's credential to a Principal and stores it (plus, on the operator
// carrier, the legacy operator identity). A missing/garbage/unknown/wrong/expired credential is a
// uniform 401; a store error is a 500. last_used_at is bumped async and never blocks the request.
func Auth(db *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			// Carrier 1: Authorization: Bearer <token> — the machine path.
			if raw, ok := bearer(r); ok {
				if strings.HasPrefix(raw, apitoken.TokenPrefix) {
					tok, ok, err := apitoken.Resolve(ctx, db, raw)
					if err != nil {
						WriteErr(w, r, http.StatusInternalServerError, "internal", "auth lookup failed")
						return
					}
					if !ok {
						unauthorized(w, r)
						return
					}
					go func(id int64) { _ = apitoken.TouchLastUsed(context.Background(), db, id) }(tok.ID)
					pr := Principal{Kind: KindBearer, ID: tok.TokenID, Label: tok.Label, Scopes: tok.Scopes}
					admitPrincipal(w, r, next, ctx, pr)
					return
				}
				// Legacy operator_key bearer fallback. W7 gates this to the loopback listener via
				// ListenerOriginFrom; W2 keeps it ungated (public hardening is not this wave).
				op, ok, err := operator.Authenticate(ctx, db, raw)
				if err != nil {
					WriteErr(w, r, http.StatusInternalServerError, "internal", "auth lookup failed")
					return
				}
				if !ok {
					unauthorized(w, r)
					return
				}
				go func() { _ = operator.TouchLastUsed(context.Background(), db, op.KeyID) }()
				pr := Principal{Kind: KindOperator, ID: strconv.FormatInt(op.KeyID, 10), Label: op.Label, Scopes: implicitFullScopes(), IsAdmin: op.IsAdmin}
				ctx = setOperator(ctx, op)
				admitPrincipal(w, r, next, ctx, pr)
				return
			}

			// Carrier 2: Authorization: Basic <b64> — human path for non-browser clients (curl -u,
			// Home-Assistant, .netrc). Verified against admin_users identically to POST /api/session
			// (dummy-verify, uniform 401), under the global argon2 semaphore + positive cache (delta-4).
			// NO WWW-Authenticate is EVER set on the 401 — a challenge would make the browser cache Basic
			// creds origin-wide and turn Basic into an ambient (CSRF-able) credential. Honoured on every
			// listener incl. loopback (tunnel-local, harmless; delta-4 §Autonome Festlegungen).
			if hasBasicScheme(r) {
				user, pass, ok := r.BasicAuth()
				if !ok {
					unauthorized(w, r)
					return
				}
				pr, status := resolveBasic(ctx, db, user, pass, RemoteIP(r))
				switch status {
				case http.StatusOK:
					admitPrincipal(w, r, next, ctx, pr)
				case http.StatusTooManyRequests:
					w.Header().Set("Retry-After", "1")
					WriteErr(w, r, http.StatusTooManyRequests, "rate_limited", "authentication capacity exceeded; retry shortly")
				case http.StatusInternalServerError:
					WriteErr(w, r, http.StatusInternalServerError, "internal", "auth lookup failed")
				default:
					unauthorized(w, r)
				}
				return
			}

			// Carrier 3: Cookie ppk_sid — the human path.
			if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
				pr, ok, err := resolveSession(ctx, db, c.Value)
				if err != nil {
					WriteErr(w, r, http.StatusInternalServerError, "internal", "auth lookup failed")
					return
				}
				if !ok {
					unauthorized(w, r)
					return
				}
				// CSRF gate — the second layer beside SameSite=Strict (design §4.1), cookie carrier ONLY.
				// A miss is a 403 with its own code, NOT the uniform 401: the client IS authenticated —
				// this is a request-shape failure, not a credential failure, and folding it into the 401
				// family would only obscure a misconfigured legitimate client.
				if !csrfSafeMethod(r.Method) && r.Header.Get(csrfHeader) != csrfHeaderValue {
					WriteErr(w, r, http.StatusForbidden, "csrf_required",
						"mutating session requests must send "+csrfHeader+": "+csrfHeaderValue)
					return
				}
				admitPrincipal(w, r, next, ctx, pr)
				return
			}

			// Carrier 4: nothing.
			unauthorized(w, r)
		})
	}
}

// resolveSession turns a cookie secret into a session Principal: resolve the (unexpired) session,
// then look up its account for the identity + admin flag. An unknown/expired session, a missing
// account, or a disabled account all resolve as unauthenticated (fail closed).
func resolveSession(ctx context.Context, db *pgxpool.Pool, secret string) (Principal, bool, error) {
	sess, ok, err := adminsession.Resolve(ctx, db, secret)
	if err != nil || !ok {
		return Principal{}, false, err
	}
	u, err := adminuser.GetByID(ctx, db, sess.UserID)
	if errors.Is(err, adminuser.ErrNotFound) {
		return Principal{}, false, nil
	}
	if err != nil {
		return Principal{}, false, err
	}
	if u.DisabledAt != nil {
		return Principal{}, false, nil
	}
	return Principal{
		Kind:    KindSession,
		ID:      strconv.FormatInt(u.ID, 10),
		Label:   u.Username,
		Scopes:  implicitFullScopes(),
		IsAdmin: u.IsAdmin,
	}, true, nil
}

// RequireAdmin rejects a non-admin Principal with 403. Compose as Auth(db)(RequireAdmin(h)).
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pr, ok := PrincipalFrom(r.Context())
		if !ok || !pr.IsAdmin {
			WriteErr(w, r, http.StatusForbidden, "forbidden", "admin privilege required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireScope rejects a Principal that lacks the given scope with 403. It sits beside RequireAdmin:
// image routes gate on a scope (image:read / image:write), fleet-admin routes gate on IsAdmin. A
// missing Principal (auth not run) fails closed. Compose as Auth(db)(RequireScope(scope)(h)).
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pr, ok := PrincipalFrom(r.Context())
			if !ok || !pr.HasScope(scope) {
				WriteErr(w, r, http.StatusForbidden, "forbidden", "missing required scope: "+scope)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
