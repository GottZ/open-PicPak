package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminaudit"
	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/apitoken"
)

type tokenHandlers struct{ pool *pgxpool.Pool }

// registerTokenRoutes mounts the machine-token admin surface (design §4.5, W6): mint, list and
// soft-revoke of api_tokens. Every route is IsAdmin-gated (RequireAdmin) — only a full operator mints
// machine credentials; an api_token itself is structurally never admin (no IsAdmin field), so a leaked
// image token can never mint or revoke another (design §5 B3). Single wiring source so main.go and the
// gating test can never drift (mirror of registerFaasRoutes / registerAuthRoutes).
func registerTokenRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := tokenHandlers{pool: pool}
	mux.Handle("POST /api/tokens", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.mint))))
	mux.Handle("GET /api/tokens", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.list))))
	mux.Handle("DELETE /api/tokens/{id}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.revoke))))
}

// mint — POST /api/tokens (IsAdmin): generate a fresh token_id+secret, store only sha256(secret), and
// return the full ppk_ token EXACTLY ONCE (once-shown, design §4.5 / S7). The response struct carries no
// secret hash — it structurally cannot leak on any later read. Unknown scopes are a 422 before the
// insert. created_by is set only when the minter authenticated as an operator_key (the FK targets
// operator_keys, and a session/basic admin id would violate it); the full who is captured in the audit
// row regardless. token.mint is written SYNCHRONOUSLY — it is as security-relevant as revoke, so a crash
// must not swallow it (design §4.6).
func (h tokenHandlers) mint(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label     string   `json:"label"`
		Scopes    []string `json:"scopes"`
		ExpiresAt *string  `json:"expires_at"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if body.Label == "" {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_label", "label is required")
		return
	}
	var expiresAt *time.Time
	if body.ExpiresAt != nil && *body.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *body.ExpiresAt)
		if err != nil {
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_expiry", "expires_at must be RFC3339")
			return
		}
		expiresAt = &t
	}

	pr, _ := adminhttp.PrincipalFrom(r.Context())
	plaintext, tok, err := apitoken.Create(r.Context(), h.pool, body.Label, body.Scopes, expiresAt, mintedBy(pr))
	if errors.Is(err, apitoken.ErrUnknownScope) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unknown_scope", err.Error())
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "token mint failed")
		return
	}

	// Synchronous audit: a mint is security-relevant (design §4.6). target is the token_id (never the
	// plaintext secret); actor is the minting principal.
	if err := adminaudit.Write(r.Context(), h.pool, adminaudit.Entry{
		ActorKind: string(pr.Kind), ActorID: pr.ID, Action: "token.mint",
		Target: tok.TokenID, RemoteIP: adminhttp.RemoteIP(r), OK: true,
	}); err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "token mint failed")
		return
	}

	adminhttp.WriteOK(w, r, map[string]any{
		"token":      plaintext, // shown ONCE — never recoverable, never stored
		"id":         tok.ID,
		"token_id":   tok.TokenID,
		"label":      tok.Label,
		"scopes":     tok.Scopes,
		"expires_at": tok.ExpiresAt,
		"created_at": tok.CreatedAt,
	})
}

// list — GET /api/tokens (IsAdmin): every token WITHOUT any secret (design §4.5, the listOperators
// pattern). apitoken.List never projects secret_hash, so the response structurally cannot carry token
// material; status is derived here from disabled_at/expires_at.
func (h tokenHandlers) list(w http.ResponseWriter, r *http.Request) {
	toks, err := apitoken.List(r.Context(), h.pool)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "token list failed")
		return
	}
	out := make([]map[string]any, 0, len(toks))
	for _, t := range toks {
		out = append(out, map[string]any{
			"id":         t.ID,
			"token_id":   t.TokenID,
			"label":      t.Label,
			"scopes":     t.Scopes,
			"created_at": t.CreatedAt,
			"expires_at": t.ExpiresAt,
			"last_used":  t.LastUsedAt,
			"status":     tokenStatus(t),
		})
	}
	adminhttp.WriteOK(w, r, map[string]any{"tokens": out})
}

// revoke — DELETE /api/tokens/{id} (IsAdmin): soft-revoke (disabled_at=now()); the token authenticates
// as absent from its next request on (SEC-M1). The token_id is fetched first so the audit target names
// it and an unknown id is a uniform 404. token.revoke is written SYNCHRONOUSLY (design §4.6).
func (h tokenHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such token")
		return
	}
	tok, err := apitoken.Get(r.Context(), h.pool, id)
	if errors.Is(err, apitoken.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such token")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "token revoke failed")
		return
	}
	if err := apitoken.Revoke(r.Context(), h.pool, id); err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "token revoke failed")
		return
	}

	pr, _ := adminhttp.PrincipalFrom(r.Context())
	if err := adminaudit.Write(r.Context(), h.pool, adminaudit.Entry{
		ActorKind: string(pr.Kind), ActorID: pr.ID, Action: "token.revoke",
		Target: tok.TokenID, RemoteIP: adminhttp.RemoteIP(r), OK: true,
	}); err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "token revoke failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"revoked": tok.TokenID})
}

// mintedBy returns the operator_keys id to attribute a mint to, or nil. Only the operator carrier holds
// an operator_keys id; a session/basic admin's id lives in admin_users, and passing it would violate the
// created_by → operator_keys FK. The audit trail carries the true actor for every carrier regardless.
func mintedBy(pr adminhttp.Principal) *int64 {
	if pr.Kind != adminhttp.KindOperator {
		return nil
	}
	id, err := strconv.ParseInt(pr.ID, 10, 64)
	if err != nil {
		return nil
	}
	return &id
}

// tokenStatus derives the display status of a token row from its soft-revoke stamp and expiry.
func tokenStatus(t apitoken.Token) string {
	switch {
	case t.DisabledAt != nil:
		return "revoked"
	case t.ExpiresAt != nil && !t.ExpiresAt.After(time.Now()):
		return "expired"
	default:
		return "active"
	}
}
