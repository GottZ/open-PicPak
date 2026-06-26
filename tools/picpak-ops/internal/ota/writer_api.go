package ota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AdminAPIWriter is the default/preferred write backend: an HTTP client for the Admin-
// API contract (design 07 §Admin-API write contract). The backend owns blob storage,
// re-verifies the sha, and enforces FK/precedence integrity; this client only marshals
// requests, attaches the bearer token, and maps typed errors. The Admin-API does not
// exist yet, so this is unit-tested against an httptest.Server mock.
//
// Air-gap: the resolved base URL (host-bearing) and the bearer token (secret) are held
// here but NEVER logged — request errors carry the operation name and HTTP status only,
// never the URL or token.
type AdminAPIWriter struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewAdminAPIWriter builds the client. timeout bounds every call including the multipart
// blob upload (one canonical backend.timeout; design 07 deliberately does not fork it).
func NewAdminAPIWriter(baseURL, token string, timeout time.Duration) *AdminAPIWriter {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &AdminAPIWriter{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

func (w *AdminAPIWriter) IsDirect() bool { return false }
func (w *AdminAPIWriter) Close() error   { return nil }

// apiError is the typed JSON error body the contract specifies ({error, code}).
type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// RegisterFirmware POSTs multipart/form-data to /admin/firmware (version, sha256,
// size_bytes, notes, blob). The sha is validated lowercase before the request so an
// uppercase digest never leaves the tool.
func (w *AdminAPIWriter) RegisterFirmware(ctx context.Context, req RegisterFirmware) error {
	if !ValidSHA256(req.SHA256) {
		return ErrBadSHA
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fields := map[string]string{
		"version":    req.Version,
		"sha256":     req.SHA256,
		"size_bytes": strconv.FormatInt(req.SizeBytes, 10),
		"notes":      req.Notes,
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return fmt.Errorf("ota: building register request: %w", err)
		}
	}
	part, err := mw.CreateFormFile("blob", "firmware.bin")
	if err != nil {
		return fmt.Errorf("ota: building register request: %w", err)
	}
	if _, err := part.Write(req.Blob); err != nil {
		return fmt.Errorf("ota: building register request: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("ota: building register request: %w", err)
	}
	return w.do(ctx, http.MethodPost, "/admin/firmware", mw.FormDataContentType(), &buf, "register firmware")
}

// SetChannelDefault PUTs /admin/channels/{name} with {default_version}.
func (w *AdminAPIWriter) SetChannelDefault(ctx context.Context, req SetChannelDefault) error {
	body := map[string]string{"default_version": req.Version}
	return w.doJSON(ctx, http.MethodPut, "/admin/channels/"+pathSeg(req.Channel), body, "set channel default")
}

// PinRollout POSTs /admin/rollouts with {serial, channel, version, pinned}.
func (w *AdminAPIWriter) PinRollout(ctx context.Context, req PinRollout) error {
	body := map[string]any{
		"serial":  req.Serial,
		"channel": req.Channel,
		"version": req.Version,
		"pinned":  req.Pinned,
	}
	return w.doJSON(ctx, http.MethodPost, "/admin/rollouts", body, "pin rollout")
}

// SetRolloutState PATCHes /admin/rollouts/{id} with {state}.
func (w *AdminAPIWriter) SetRolloutState(ctx context.Context, req SetRolloutState) error {
	if !ValidState(req.State) {
		return ErrBadState
	}
	body := map[string]string{"state": req.State}
	return w.doJSON(ctx, http.MethodPatch, "/admin/rollouts/"+strconv.FormatInt(req.ID, 10), body, "set rollout state")
}

func (w *AdminAPIWriter) doJSON(ctx context.Context, method, path string, body any, op string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("ota: marshaling %s request: %w", op, err)
	}
	return w.do(ctx, method, path, "application/json", bytes.NewReader(raw), op)
}

// do issues the request, attaches the bearer token, and maps the status to a typed
// error. It never includes the URL or token in an error (air-gap redaction).
func (w *AdminAPIWriter) do(ctx context.Context, method, path, contentType string, body io.Reader, op string) error {
	httpReq, err := http.NewRequestWithContext(ctx, method, w.baseURL+path, body)
	if err != nil {
		// The error from net/http can embed the URL; surface only the op name.
		return fmt.Errorf("ota: building the %s request failed", op)
	}
	httpReq.Header.Set("Content-Type", contentType)
	if w.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+w.token)
	}

	resp, err := w.client.Do(httpReq)
	if err != nil {
		// net/http dial errors embed host:port — never surface them.
		return fmt.Errorf("ota: the %s request did not reach the admin API (check admin_api_url/connectivity)", op)
	}
	defer resp.Body.Close()
	return mapStatus(resp, op)
}

// mapStatus maps the HTTP status to a typed error, reading the {error,code} body for
// the message where present (without leaking host/token).
func mapStatus(resp *http.Response, op string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var ae apiError
	if raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096)); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &ae)
	}
	switch resp.StatusCode {
	case http.StatusConflict: // 409
		return ErrVersionExists
	case http.StatusUnprocessableEntity: // 422
		return ErrSHAMismatch
	case http.StatusNotFound: // 404
		return ErrUnknownRef
	default:
		if ae.Error != "" {
			return fmt.Errorf("ota: %s failed (HTTP %d): %s", op, resp.StatusCode, ae.Error)
		}
		return fmt.Errorf("ota: %s failed (HTTP %d)", op, resp.StatusCode)
	}
}

// pathSeg escapes a channel name for use as a path segment without pulling net/url for
// one field (channel names are simple identifiers; a stray '/' is the only hazard).
func pathSeg(s string) string {
	return strings.ReplaceAll(s, "/", "%2F")
}
