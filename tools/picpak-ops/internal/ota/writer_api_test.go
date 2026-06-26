package ota

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const goodSHA = "ae38392fbed513cecfee4049aafddb38df364f38cf08d7325b50a8fe9e1e2f0f"

// TestAdminAPIWriter_RegisterFirmware_Contract mocks the Admin-API with an
// httptest.Server and asserts the request shape the backend will receive: POST
// /admin/firmware, multipart body carrying version/sha256/size_bytes/notes + the blob
// file, and the bearer token header. The API does not exist yet, so the mock IS the
// contract.
func TestAdminAPIWriter_RegisterFirmware_Contract(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotVersion, gotSHA, gotSize string
	var gotBlob []byte

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(rw, "bad multipart", http.StatusBadRequest)
			return
		}
		gotVersion = r.FormValue("version")
		gotSHA = r.FormValue("sha256")
		gotSize = r.FormValue("size_bytes")
		if f, _, err := r.FormFile("blob"); err == nil {
			gotBlob, _ = io.ReadAll(f)
			f.Close()
		}
		rw.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	w := NewAdminAPIWriter(srv.URL+"/", "secret-token", 5*time.Second)
	err := w.RegisterFirmware(context.Background(), RegisterFirmware{
		Version:   "0.6.2",
		SHA256:    goodSHA,
		SizeBytes: 1234,
		Notes:     "first build",
		Blob:      []byte("firmware-bytes"),
	})
	if err != nil {
		t.Fatalf("RegisterFirmware: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/admin/firmware" {
		t.Errorf("path = %q, want /admin/firmware", gotPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("auth = %q, want Bearer secret-token", gotAuth)
	}
	if gotVersion != "0.6.2" || gotSHA != goodSHA || gotSize != "1234" {
		t.Errorf("fields version=%q sha=%q size=%q", gotVersion, gotSHA, gotSize)
	}
	if string(gotBlob) != "firmware-bytes" {
		t.Errorf("blob = %q, want firmware-bytes", string(gotBlob))
	}
}

// TestAdminAPIWriter_RejectsUppercaseSHA proves the tool fail-closes an uppercase sha
// BEFORE any HTTP request (the server is never hit).
func TestAdminAPIWriter_RejectsUppercaseSHA(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { hit = true }))
	defer srv.Close()

	w := NewAdminAPIWriter(srv.URL, "", 2*time.Second)
	err := w.RegisterFirmware(context.Background(), RegisterFirmware{
		Version: "0.6.2",
		SHA256:  strings.ToUpper(goodSHA), // uppercase: rejected by the tool
		Blob:    []byte("x"),
	})
	if !errors.Is(err, ErrBadSHA) {
		t.Fatalf("expected ErrBadSHA, got %v", err)
	}
	if hit {
		t.Fatal("an uppercase sha must be rejected before any HTTP request")
	}
}

// TestAdminAPIWriter_ErrorMapping proves the 409/422/404 → typed-error mapping the
// pane relies on for one consistent message regardless of backend.
func TestAdminAPIWriter_ErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusConflict, ErrVersionExists},
		{http.StatusUnprocessableEntity, ErrSHAMismatch},
		{http.StatusNotFound, ErrUnknownRef},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(tc.status)
			_, _ = rw.Write([]byte(`{"error":"x","code":"y"}`))
		}))
		w := NewAdminAPIWriter(srv.URL, "t", 2*time.Second)
		err := w.SetChannelDefault(context.Background(), SetChannelDefault{Channel: "stable", Version: "0.6.2"})
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d → %v, want %v", tc.status, err, tc.want)
		}
		srv.Close()
	}
}

// TestAdminAPIWriter_RolloutContract asserts the pin + state requests' method/path/body.
func TestAdminAPIWriter_RolloutContract(t *testing.T) {
	var pinMethod, pinPath, pinBody, stateMethod, statePath, stateBody string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/admin/rollouts":
			pinMethod, pinPath, pinBody = r.Method, r.URL.Path, string(raw)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/admin/rollouts/"):
			stateMethod, statePath, stateBody = r.Method, r.URL.Path, string(raw)
		default:
			http.Error(rw, "unexpected", http.StatusBadRequest)
			return
		}
		rw.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := NewAdminAPIWriter(srv.URL, "", 2*time.Second)
	if err := w.PinRollout(context.Background(), PinRollout{Serial: "*", Channel: "stable", Version: "0.6.2", Pinned: true}); err != nil {
		t.Fatalf("PinRollout: %v", err)
	}
	if pinMethod != http.MethodPost || pinPath != "/admin/rollouts" {
		t.Errorf("pin %s %s, want POST /admin/rollouts", pinMethod, pinPath)
	}
	if !strings.Contains(pinBody, `"serial":"*"`) || !strings.Contains(pinBody, `"pinned":true`) {
		t.Errorf("pin body = %s", pinBody)
	}

	if err := w.SetRolloutState(context.Background(), SetRolloutState{ID: 7, State: StatePaused}); err != nil {
		t.Fatalf("SetRolloutState: %v", err)
	}
	if stateMethod != http.MethodPatch || statePath != "/admin/rollouts/7" {
		t.Errorf("state %s %s, want PATCH /admin/rollouts/7", stateMethod, statePath)
	}
	if !strings.Contains(stateBody, `"state":"paused"`) {
		t.Errorf("state body = %s", stateBody)
	}

	// A bad state is rejected before any request.
	if err := w.SetRolloutState(context.Background(), SetRolloutState{ID: 7, State: "bogus"}); !errors.Is(err, ErrBadState) {
		t.Errorf("bad state → %v, want ErrBadState", err)
	}
}
