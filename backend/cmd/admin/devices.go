package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/devicestore"
)

type deviceHandlers struct{ pool *pgxpool.Pool }

// list — GET /api/devices (auth): identity + membership + bond state for the whole fleet.
func (h deviceHandlers) list(w http.ResponseWriter, r *http.Request) {
	rows, err := devicestore.List(r.Context(), h.pool)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "device list failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"devices": rows})
}

// get — GET /api/devices/{serial} (auth): one device + auth/bond state (pubkey_present vs bonded).
func (h deviceHandlers) get(w http.ResponseWriter, r *http.Request) {
	d, found, err := devicestore.Get(r.Context(), h.pool, r.PathValue("serial"))
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "device lookup failed")
		return
	}
	if !found {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such device")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"device": d})
}

// register — POST /api/devices (admin): register/re-bond. All validation runs BEFORE the DB (→ 422,
// never 500). The pubkey is the single bond field; the private key is never accepted.
func (h deviceHandlers) register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Serial      string  `json:"serial"`
		Label       *string `json:"label"`
		Channel     *string `json:"channel"`
		EcdsaPubkey string  `json:"ecdsa_pubkey"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if !devicestore.ValidSerial(body.Serial) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial",
			`serial must match ^[A-Za-z0-9_-]{1,31}$ and not be '*'`)
		return
	}
	pubkey, err := base64.StdEncoding.DecodeString(body.EcdsaPubkey)
	if err != nil || !devicestore.ValidPubkey(pubkey) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_pubkey",
			"ecdsa_pubkey must be a base64-encoded 65-byte uncompressed P-256 point (0x04||X||Y)")
		return
	}
	action, err := devicestore.Register(r.Context(), h.pool, body.Serial, body.Label, body.Channel, pubkey)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" { // FK violation → unknown channel
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unknown_channel",
				"channel does not exist")
			return
		}
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "register failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"serial": body.Serial, "action": action})
}
