package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/open-picpak/backend/internal/bwry"
	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
	"github.com/open-picpak/backend/internal/sealbox"
	"github.com/open-picpak/backend/internal/secrets"
)

// SecretMode selects how bound secrets are resolved: real values (build_frame) or stub placeholders
// (Doc 25 test-run — no plaintext leaves the DB).
type SecretMode int

const (
	SecretsReal SecretMode = iota
	SecretsStub
)

// RenderOpts carries the only divergences between renderOnce's callers, so there is no logic fork
// (K7). EgressRegister/EgressUnregister, when set, bind this call's egress_allow to a per-call
// credential at the proxy (least privilege, D24.8); no-egress functions leave them nil.
type RenderOpts struct {
	Secrets SecretMode
	Limits  faasproto.Limits
	WantRaw bool
	Force   bool
	// Dither is the TRUSTED per-function dither selector — the operator's trigger_config.dither
	// (Policy=Data, A31.2). It is resolved at pack time and, when it parses, WINS over the worker's
	// echoed return value (see resolveDitherStr). Empty for callers with no per-function policy, which
	// then fall through to the worker return and finally DitherDefault (A31-E2 back-compat).
	Dither           string
	DitherDefault    string
	M4Sock           string
	Timeout          time.Duration
	EgressRegister   func(cred string, allow []string) error
	EgressUnregister func(cred string)
	// Input is the source-image bytes for the built-in __playlist source (A27 W3). It rides the M4
	// request's additive `input` field (proto v2); empty for every operator render, so those marshal
	// unchanged. The request-direction MaxRequestFrame ceiling (not MaxFrame) bounds it (K7).
	Input []byte
}

// RenderResult is the persistence-free render output: packed BWRY (nil on err), the pre-pack raw RGB
// (only when WantRaw, for Doc 25's preview), the worker/supervisor meta, and the error (nil on ok).
type RenderResult struct {
	Packed []byte
	Raw    []byte
	Meta   faasproto.ResponseMeta
	Err    *faasproto.RenderErr
}

// resolveSecrets maps the function's BOUND names to VALUES (least privilege — only bound names reach
// the worker, D24.4/T10). Two-class per D24.5/T9: a missing secret (ErrSecretNotFound) is injected
// ABSENT (fail-open — the render proceeds), while a hard sealbox.Open fault ABORTS the render (err
// kind "secret") and is NEVER masked as not-found.
func resolveSecrets(ctx context.Context, q secrets.Querier, box *sealbox.Box, names []string, mode SecretMode) (map[string]string, *faasproto.RenderErr) {
	out := make(map[string]string, len(names))
	for _, name := range names {
		if mode == SecretsStub {
			out[name] = "<secret:" + name + ">"
			continue
		}
		val, err := secrets.ResolveSecret(ctx, q, box, name)
		switch {
		case errors.Is(err, secrets.ErrSecretNotFound):
			continue // injected absent — the function sees secrets.<name> undefined (fail-open)
		case err != nil:
			return nil, &faasproto.RenderErr{Kind: "secret", Msg: "secret " + name + " failed to open"}
		default:
			out[name] = string(val)
		}
	}
	return out, nil
}

// resolveDitherStr selects the pack-time dither mode by TRUST-ORDERED precedence (A31.2, design 31
// §4.2/§7) and returns its canonical token. trigger is the trusted per-function trigger_config.dither
// (operator policy, Policy=Data); ret is the worker's echoed dither (UNTRUSTED — it originates in the
// sandboxed function JS, ResponseMeta.Dither); def is the global DITHER_DEFAULT. The trusted trigger
// wins whenever it parses. The untrusted return is tolerated ONLY as a fallback below it, NEVER as an
// override: letting the worker's return beat trigger_config would let untrusted code overwrite trusted
// operator policy (a Policy=Data breach). That fallback is safe only because ParseDither is fail-closed
// and the selector space is a fixed set of deterministic, secret-free pack algorithms — an unparsable
// value at any tier simply falls through. The final tier reproduces today's byte-exact frame when
// DITHER_DEFAULT is "none"/unset (A31-E2 back-compat: no per-function dither ⇒ "" ⇒ none).
func resolveDitherStr(trigger, ret, def string) string {
	for _, s := range []string{trigger, ret, def} {
		if _, ok := bwry.ParseDither(s); ok {
			return s // ParseDither only accepts canonical tokens, so s is already canonical
		}
	}
	return "none"
}

// renderOnce is the shared, persistence-free primitive: resolve secrets → drive the worker over M4 →
// pack BWRY. It touches no cache/last-good/DB. build_frame (W4b) wraps it with the cache/fallback
// layer; Doc 25's test-run calls it with {SecretsStub, clamped Limits, WantRaw}.
func renderOnce(ctx context.Context, q secrets.Querier, box *sealbox.Box, fn *faasstore.Function, rctx faasproto.RequestCtx, opts RenderOpts) RenderResult {
	secretVals, serr := resolveSecrets(ctx, q, box, fn.SecretBindings, opts.Secrets)
	if serr != nil {
		return RenderResult{Meta: faasproto.ResponseMeta{V: 1, OK: false, Err: serr}, Err: serr}
	}

	cred := newCallID()
	if opts.EgressRegister != nil && len(fn.EgressAllow) > 0 {
		if err := opts.EgressRegister(cred, fn.EgressAllow); err != nil {
			re := &faasproto.RenderErr{Kind: "egress", Msg: "egress provisioning failed"}
			return RenderResult{Meta: faasproto.ResponseMeta{V: 1, OK: false, Err: re}, Err: re}
		}
		if opts.EgressUnregister != nil {
			defer opts.EgressUnregister(cred)
		}
	}

	req := faasproto.Request{
		V:             1,
		ID:            newCallID(),
		Fn:            faasproto.RequestFn{ID: fn.ID, Ver: fn.Version, Source: fn.Source},
		Ctx:           rctx,
		Force:         opts.Force,
		DitherDefault: opts.DitherDefault,
		Secrets:       secretVals,
		EgressAllow:   fn.EgressAllow,
		EgressCred:    cred,
		Limits:        opts.Limits,
		Input:         opts.Input,
	}

	meta, raw, err := driveWorker(opts.M4Sock, req, opts.Timeout)
	if err != nil {
		// a transport failure (dial/read/deadline) is a timeout-class fault → fallback upstream.
		re := &faasproto.RenderErr{Kind: "timeout", Msg: err.Error()}
		return RenderResult{Meta: faasproto.ResponseMeta{V: 1, ID: req.ID, OK: false, Err: re}, Err: re}
	}
	if meta.Err != nil {
		return RenderResult{Meta: meta, Err: meta.Err} // worker-side throw/timeout/egress/offsize/oom
	}

	img, ferr := bwry.FromRGB(raw)
	if ferr != nil {
		re := &faasproto.RenderErr{Kind: "pack", Msg: ferr.Error()}
		return RenderResult{Meta: meta, Err: re}
	}
	// Pack-time dither resolution (A31.2): the trusted trigger_config selector (opts.Dither) wins over
	// the untrusted worker return (meta.Dither), which is itself only a fallback above DITHER_DEFAULT.
	dith := resolveDitherStr(opts.Dither, meta.Dither, opts.DitherDefault)
	dmode, _ := bwry.ParseDither(dith) // always ok — resolveDitherStr returns a canonical token
	packed, perr := bwry.PackWithDither(img, dmode)
	if perr != nil {
		re := &faasproto.RenderErr{Kind: "pack", Msg: perr.Error()}
		return RenderResult{Meta: meta, Err: re}
	}
	meta.Dither = dith // report the mode ACTUALLY packed (trusted-resolved), not the untrusted echo
	res := RenderResult{Packed: packed, Meta: meta}
	if opts.WantRaw {
		res.Raw = raw
	}
	return res
}

// driveWorker performs one M4 exchange: dial the worker UDS, write the render-request, read the
// render-response, and enforce a wall-clock deadline. The worker itself always answers within
// limits.timeout_ms (its slot is killed on overrun), so this deadline is a generous outer bound.
func driveWorker(sockPath string, req faasproto.Request, timeout time.Duration) (faasproto.ResponseMeta, []byte, error) {
	conn, err := net.DialTimeout("unix", sockPath, timeout)
	if err != nil {
		return faasproto.ResponseMeta{}, nil, fmt.Errorf("m4 dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	body, err := json.Marshal(req)
	if err != nil {
		return faasproto.ResponseMeta{}, nil, err
	}
	if err := faasproto.WriteFrame(conn, faasproto.KindRenderRequest, body); err != nil {
		return faasproto.ResponseMeta{}, nil, fmt.Errorf("m4 write: %w", err)
	}
	kind, payload, err := faasproto.ReadFrame(conn)
	if err != nil {
		return faasproto.ResponseMeta{}, nil, fmt.Errorf("m4 read: %w", err)
	}
	if kind != faasproto.KindRenderResponse {
		return faasproto.ResponseMeta{}, nil, fmt.Errorf("m4: unexpected kind %#x", kind)
	}
	return faasproto.DecodeResponse(payload)
}

func newCallID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
