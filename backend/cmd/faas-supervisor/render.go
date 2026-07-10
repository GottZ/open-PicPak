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
	Secrets          SecretMode
	Limits           faasproto.Limits
	WantRaw          bool
	Force            bool
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
	packed, perr := bwry.Pack(img)
	if perr != nil {
		re := &faasproto.RenderErr{Kind: "pack", Msg: perr.Error()}
		return RenderResult{Meta: meta, Err: re}
	}
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
