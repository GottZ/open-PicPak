package faascore

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/open-picpak/backend/internal/bwry"
	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
)

// testrender.go is the Doc 25 test-render arm — the ONE new execution path A25 adds to the supervisor. It
// reuses renderOnce (the K7 shared primitive: production worker-drive + BWRY pack + secret-resolve), but:
//   - persistence-FREE: renderOnce touches no cache/last-good/fallback (buildFrame wraps those), so a test
//     is side-effect-free on the served fleet by construction (D25.4, T5) — a test never writes a frame a
//     real device would then serve.
//   - STUB secrets: SecretsStub injects "<secret:name>" markers, never a plaintext value (D25.12a, T15).
//   - CLAMPED limits: a test may ask for LESS than a prod render, never more (D25.4, T16) — it shares the
//     prod worker pool, so an unclamped limit would starve real renders.
// It lives on a SEPARATE UDS (FAAS_TEST_SOCK, admin↔supervisor on dbnet — main.go), distinct from the
// ingest render seam; cmd/admin gates it with requireAdmin + attribution (D25.3).

// testRenderReq is the admin→supervisor seam body. admin has already resolved ctx.channel server-side
// (D20.5) and gated on requireAdmin; this internal UDS just executes. Either ID (a saved function) or the
// ad-hoc draft fields (unsaved source — D25.3, no new privilege beyond what POST+PATCH+poll already give).
type testRenderReq struct {
	ID             *int64            `json:"id"`
	Source         string            `json:"source"`
	Dither         string            `json:"dither"`
	SecretBindings []string          `json:"secret_bindings"`
	EgressAllow    []string          `json:"egress_allow"`
	Serial         string            `json:"serial"`
	Channel        string            `json:"channel"`
	Trigger        string            `json:"trigger"`
	Now            string            `json:"now"`
	Payload        json.RawMessage   `json:"payload"`
	Limits         *faasproto.Limits `json:"limits"`
}

// testMeta is the meta block prepended to the framed test-render response (§4.2 step 7). The packed block
// is ALWAYS present (an error renders the embedded error frame for the preview slot); the raw pre-pack RGB
// rides only on success (D25.5, the side-by-side "what you drew").
type testMeta struct {
	OK     bool                 `json:"ok"`
	Status string               `json:"status"` // "ok" | "error"
	Wake   int                  `json:"wake"`
	Dither string               `json:"dither"`
	RawFmt string               `json:"raw_fmt"` // "rgb" on success, "" on error
	RawW   int                  `json:"raw_w"`
	RawH   int                  `json:"raw_h"`
	Log    []faasproto.LogLine  `json:"log"`
	Err    *faasproto.RenderErr `json:"err"`
}

// clampLimits clamps a client's requested limits to the production maxima — a test can only ask for LESS
// (D25.4/T16). A zero/absent field keeps the prod default; a field above the prod max is capped, never
// raised. Pure so the clamp is unit-tested without a worker.
func clampLimits(prod faasproto.Limits, req *faasproto.Limits) faasproto.Limits {
	out := prod
	if req == nil {
		return out
	}
	if req.TimeoutMs > 0 && req.TimeoutMs < out.TimeoutMs {
		out.TimeoutMs = req.TimeoutMs
	}
	if req.MemMB > 0 && req.MemMB < out.MemMB {
		out.MemMB = req.MemMB
	}
	return out
}

// TestRenderSeam is the in-process admin→supervisor test-render seam (design 35 §3, replaces the
// FAAS_TEST_SOCK transport). It decodes the SAME seam JSON body cmd/admin builds and returns the
// BYTE-IDENTICAL framed response (u32be metaLen | meta | packed(30000) | raw) plus an HTTP-style status
// (200 ok, 404 unknown saved fn, 400 bad body). The browser-facing wire frame is unchanged (design §3);
// cmd/admin forwards `out` verbatim to the browser / parses the packed slot for a preview.
func (s *Supervisor) TestRenderSeam(ctx context.Context, seam []byte) (out []byte, status int) {
	var body testRenderReq
	if err := json.Unmarshal(seam, &body); err != nil {
		return nil, http.StatusBadRequest
	}
	return s.testRender(ctx, body)
}

// handleTestRender is the thin HTTP wrapper retained for the in-package tests. Production reaches
// TestRenderSeam() directly (no test.sock).
func (s *Supervisor) handleTestRender(w http.ResponseWriter, r *http.Request) {
	var body testRenderReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&body); err != nil {
		http.Error(w, "bad test-render request", http.StatusBadRequest)
		return
	}
	out, status := s.testRender(r.Context(), body)
	if status != http.StatusOK {
		http.Error(w, "no such function", status)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// testRender executes one test render and returns the framed response bytes + an HTTP-style status. It
// NEVER writes cache/last-good/DB (D25.4). Shared by the HTTP wrapper and the in-process seam.
func (s *Supervisor) testRender(ctx context.Context, body testRenderReq) (out []byte, status int) {
	// resolve the function: a saved row (by id) or the ad-hoc draft (unsaved source — D25.3).
	var fn *faasstore.Function
	if body.ID != nil {
		f, err := faasstore.LoadFunction(ctx, s.pool, *body.ID)
		if err != nil {
			return nil, http.StatusNotFound
		}
		fn = f
	} else {
		fn = &faasstore.Function{
			Source:         body.Source,
			TriggerType:    faasstore.TriggerType(triggerOrRender(body.Trigger)),
			SecretBindings: body.SecretBindings,
			EgressAllow:    body.EgressAllow,
		}
	}

	// effective test-run dither knob (trusted): body.Dither is the ad-hoc top tier (the operator's
	// test-UI knob, D25.3 — no new privilege), then the saved fn's trigger_config.dither. renderOnce
	// resolves this trusted knob against the worker return and DITHER_DEFAULT with the SAME precedence
	// as production build_frame (A31.2 / resolveDitherStr), so the preview is faithful to what a device
	// would be served — the perfn-dither gap is closed: production doRender now wires trigger_config too.
	trusted := body.Dither
	if trusted == "" && body.ID != nil {
		trusted = parseTriggerConfig(fn.TriggerConfig).Dither
	}

	lim := clampLimits(s.limits, body.Limits)

	rctx := faasproto.RequestCtx{
		Serial:  body.Serial,
		Channel: body.Channel,
		Trigger: faasproto.Trigger{Type: triggerOrRender(body.Trigger), Payload: body.Payload},
		Now:     nowOrDefault(body.Now),
	}

	res := renderOnce(ctx, s.pool, s.box, fn, rctx, RenderOpts{
		Secrets:          SecretsStub, // D25.12a — never a plaintext value leaves the DB
		Limits:           lim,
		WantRaw:          true,    // the pre-pack RGB for the side-by-side preview (D25.5)
		Force:            true,    // no cache short-circuit (D25.4)
		Dither:           trusted, // trusted knob; renderOnce resolves precedence exactly as production
		DitherDefault:    s.ditherDefault,
		M4Sock:           s.m4Sock,
		Timeout:          time.Duration(lim.TimeoutMs)*time.Millisecond + 10*time.Second,
		EgressRegister:   s.egress.register, // the SAME egress guard as prod (T10 — a denial surfaces)
		EgressUnregister: s.egress.unregister,
	})

	// D25.12b redaction belt: mask any resolved bound-secret VALUE out of log[]/err.msg BEFORE the frame
	// leaves the process. Under the stub default there is no real value (the injected markers are safe and
	// MUST stay visible, T15a), so the map is empty → no-op; the belt covers the deferred real-value mode.
	redactSecretValues(&res.Meta, nil)

	meta := testMeta{
		OK:     res.Err == nil,
		Status: statusOf(res.Err),
		Wake:   s.wakeFor(res.Meta),
		Dither: res.Meta.Dither, // the mode ACTUALLY packed (trusted-resolved by renderOnce), not the raw request knob
		Log:    res.Meta.Log,
		Err:    res.Err,
	}
	packed := res.Packed
	var raw []byte
	if res.Err == nil {
		raw = res.Raw
		meta.RawFmt = "rgb"
		meta.RawW = bwry.Width
		meta.RawH = bwry.Height
	} else {
		packed = errorFrame // always a valid 30000-byte frame for the preview slot (§4.2 step 7)
	}
	frame, err := encodeTestFrame(meta, packed, raw)
	if err != nil {
		return nil, http.StatusInternalServerError
	}
	return frame, http.StatusOK
}

func statusOf(err *faasproto.RenderErr) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// encodeTestFrame builds u32be metaLen | meta JSON | packed | raw — the browser-facing wire frame
// (bwrydecode.ts). cmd/admin forwards it verbatim to the browser or parses the packed slot for a preview.
func encodeTestFrame(meta testMeta, packed, raw []byte) ([]byte, error) {
	mj, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 0, 4+len(mj)+len(packed)+len(raw))
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(mj)))
	buf = append(buf, hdr[:]...)
	buf = append(buf, mj...)
	buf = append(buf, packed...)
	buf = append(buf, raw...)
	return buf, nil
}

// redactSecretValues masks every resolved bound-secret VALUE out of the response log[] + err.msg by exact
// substring, BEFORE the frame leaves the supervisor (D25.12b) — a value becomes "<redacted:<name>>". The
// stub path passes no real values (its markers are safe + must stay visible, T15a); this is the belt for
// the deferred real-value mode (O8): a function that logs cap.secrets.x must never round-trip a plaintext
// to the browser (T15).
func redactSecretValues(meta *faasproto.ResponseMeta, values map[string]string) {
	if meta == nil || len(values) == 0 {
		return
	}
	mask := func(str string) string {
		for name, val := range values {
			if val == "" {
				continue
			}
			str = strings.ReplaceAll(str, val, "<redacted:"+name+">")
		}
		return str
	}
	for i := range meta.Log {
		meta.Log[i].Msg = mask(meta.Log[i].Msg)
	}
	if meta.Err != nil {
		meta.Err.Msg = mask(meta.Err.Msg)
	}
}
