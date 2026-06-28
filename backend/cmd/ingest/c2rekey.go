package main

// C2 auth re-key handshake (Doc 15): establishes a per-session HOTP secret authenticated by the
// device's long-term ECDSA P-256 key. The backend holds ONLY the public key, so a DB breach cannot
// forge a device. (P-256, not Ed25519: Ed25519 is not implemented in ESP-IDF mbedTLS; ECDSA P-256 is.)
// Flow:
//   GET  /<token>/c2/challenge?sn=X                 -> {"nonce": hex}   (single-use, short TTL)
//   POST /<token>/c2/rekey?sn=X  {nonce,secret,sig} -> 204             (ECDSA sig over sha256(msg))
// The signed message is domain-separated and binds sn + nonce + sha256(secret), so a captured
// signature cannot be replayed (nonce single-use) nor lifted to a different device/secret.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

const (
	nonceLen   = 16
	nonceTTL   = 60 * time.Second
	pubkeyLen  = 65 // uncompressed P-256 point: 0x04 || X(32) || Y(32)
	secretSize = 20
)

// rekeyMsg is the exact byte string both sides hash+sign/verify. The firmware must build it identically.
func rekeyMsg(sn, nonceHex string, secret []byte) []byte {
	h := sha256.Sum256(secret)
	return []byte("c2rekey\n" + sn + "\n" + nonceHex + "\n" + hex.EncodeToString(h[:]))
}

// verifyRekeySig checks an ECDSA P-256 (ASN.1 DER) signature over sha256(rekeyMsg). pub is the raw
// 65-byte uncompressed point. elliptic.Unmarshal rejects an off-curve / malformed point (nil x).
func verifyRekeySig(pub []byte, sn, nonceHex string, secret, sig []byte) bool {
	if len(pub) != pubkeyLen || pub[0] != 0x04 {
		return false
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), pub) //nolint:staticcheck // raw point in; ecdh has no ecdsa.Verify path
	if x == nil {
		return false
	}
	pk := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
	h := sha256.Sum256(rekeyMsg(sn, nonceHex, secret))
	return ecdsa.VerifyASN1(pk, h[:], sig)
}

func (s *server) handleC2Challenge(w http.ResponseWriter, r *http.Request) {
	sn := r.URL.Query().Get("sn")
	if sn == "" {
		http.NotFound(w, r)
		return
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	// Persist the nonce ONLY for a bonded device (the INSERT...SELECT is a no-op otherwise). We still
	// return a nonce in every case so the response shape is identical -> no device-existence oracle.
	_, _ = s.pool.Exec(r.Context(),
		`INSERT INTO c2_nonce (serial, nonce, issued_at)
		 SELECT $1, $2, now() FROM device_auth WHERE serial = $1 AND ecdsa_pubkey IS NOT NULL
		 ON CONFLICT (serial) DO UPDATE SET nonce = EXCLUDED.nonce, issued_at = now()`,
		sn, nonce)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"nonce": hex.EncodeToString(nonce)})
}

func (s *server) handleC2Rekey(w http.ResponseWriter, r *http.Request) {
	sn := r.URL.Query().Get("sn")
	if sn == "" {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Nonce  string `json:"nonce"`
		Secret string `json:"secret"`
		Sig    string `json:"sig"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}
	nonce, e1 := hex.DecodeString(body.Nonce)
	secret, e2 := hex.DecodeString(body.Secret)
	sig, e3 := hex.DecodeString(body.Sig)

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var pk, storedNonce []byte
	var issued time.Time
	// COALESCE keeps issued non-NULL when there is no pending nonce row (storedNonce stays nil -> reject).
	qErr := tx.QueryRow(ctx,
		`SELECT a.ecdsa_pubkey, n.nonce, COALESCE(n.issued_at, 'epoch'::timestamptz)
		   FROM device_auth a LEFT JOIN c2_nonce n ON n.serial = a.serial
		  WHERE a.serial = $1 FOR UPDATE OF a`,
		sn).Scan(&pk, &storedNonce, &issued)

	// Constant-shape failure: any problem -> a plain 401, no detail (no existence/replay oracle).
	ok := qErr == nil && e1 == nil && e2 == nil && e3 == nil &&
		pk != nil && len(secret) == secretSize && storedNonce != nil &&
		subtle.ConstantTimeCompare(nonce, storedNonce) == 1 &&
		time.Since(issued) < nonceTTL &&
		verifyRekeySig(pk, sn, body.Nonce, secret, sig)
	if !ok {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	// Atomic: install the new session secret, reset the counter, bump the epoch, consume the nonce.
	if _, err := tx.Exec(ctx,
		`UPDATE device_auth
		    SET session_secret = $1, session_epoch = session_epoch + 1,
		        session_last_counter = 0, session_bootstrapped = false, last_seen_at = now()
		  WHERE serial = $2`, secret, sn); err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM c2_nonce WHERE serial = $1`, sn); err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
