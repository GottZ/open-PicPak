package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/sealbox"
	"github.com/open-picpak/backend/internal/secrets"
)

// secretValueMax caps a PUT value: generous for provider keys/tokens, bounds the seal input.
const secretValueMax = 64 * 1024

type secretHandlers struct {
	pool *pgxpool.Pool
	box  *sealbox.Box
}

// put — PUT /api/secrets/{name} (admin): seal + UPSERT. Response is {name, action:"created"|"rotated"}
// and carries NO value (write-only store); the value is never echoed and never logged.
func (h secretHandlers) put(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !secrets.ValidSecretName(name) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_name",
			"name must match ^[a-z0-9][a-z0-9._-]{0,127}$")
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, secretValueMax+1024)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if body.Value == "" {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "empty_value", "value must be non-empty")
		return
	}
	if len(body.Value) > secretValueMax {
		adminhttp.WriteErr(w, r, http.StatusRequestEntityTooLarge, "value_too_large", "value exceeds the size cap")
		return
	}
	created, err := secrets.PutSecret(r.Context(), h.pool, h.box, name, []byte(body.Value))
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "secret write failed")
		return
	}
	action := "rotated"
	if created {
		action = "created"
	}
	adminhttp.WriteOK(w, r, map[string]any{"name": name, "action": action})
}

// list — GET /api/secrets (admin): metadata for every secret. Never value/ciphertext/nonce.
func (h secretHandlers) list(w http.ResponseWriter, r *http.Request) {
	metas, err := secrets.ListMeta(r.Context(), h.pool)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "secret list failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"secrets": metas})
}

// get — GET /api/secrets/{name} (admin): one secret's metadata; 404 if absent. No secret material.
func (h secretHandlers) get(w http.ResponseWriter, r *http.Request) {
	m, err := secrets.GetMeta(r.Context(), h.pool, r.PathValue("name"))
	if errors.Is(err, secrets.ErrSecretNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such secret")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "secret lookup failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"secret": m})
}

// del — DELETE /api/secrets/{name} (admin): unconditional remove (D18.6); 404 if absent.
func (h secretHandlers) del(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	found, err := secrets.DeleteSecret(r.Context(), h.pool, name)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "secret delete failed")
		return
	}
	if !found {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such secret")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"name": name})
}
