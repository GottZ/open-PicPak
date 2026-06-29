package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/commandstore"
	"github.com/open-picpak/backend/internal/devicestore"
)

type commandHandlers struct{ pool *pgxpool.Pool }

// enqueue serves both POST /api/command ({serial,script,note?}) and
// POST /api/devices/{serial}/command ({script,note?}). A path serial wins over the body. The command
// is stamped with the authenticated operator (SEC-M2 / D17.9).
func (h commandHandlers) enqueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Serial string  `json:"serial"`
		Script string  `json:"script"`
		Note   *string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, commandstore.ScriptMax+1024)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	serial := body.Serial
	if p := r.PathValue("serial"); p != "" {
		serial = p
	}
	if serial != "*" && !devicestore.ValidSerial(serial) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial",
			"serial must be a valid device serial or '*'")
		return
	}

	op, _ := adminhttp.Operator(r.Context())
	seq, err := commandstore.Enqueue(r.Context(), h.pool, serial, body.Script, body.Note, op.KeyID)
	if err != nil {
		switch {
		case errors.Is(err, commandstore.ErrScriptEmpty), errors.Is(err, commandstore.ErrScriptTooLong):
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_script", err.Error())
		case errors.Is(err, commandstore.ErrUnknownSerial):
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unknown_serial", err.Error())
		default:
			adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "enqueue failed")
		}
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"seq": seq, "serial": serial})
}
