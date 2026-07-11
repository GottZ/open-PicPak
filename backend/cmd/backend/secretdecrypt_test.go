package main

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/open-picpak/backend/internal/sealbox"
)

const testKeyHex = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"

// mimeWrap reproduces PostgreSQL encode(bytea,'base64'): RFC 2045 MIME, a newline every 76 chars.
func mimeWrap(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i += 76 {
		if i > 0 {
			b.WriteByte('\n')
		}
		end := i + 76
		if end > len(s) {
			end = len(s)
		}
		b.WriteString(s[i:end])
	}
	return b.String()
}

func sealRecord(t *testing.T, name string, value []byte) []byte {
	t.Helper()
	box, err := sealbox.New(testKeyHex, "")
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	nonce, ct, err := box.Seal(name, value)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// the exact host-side dump shape, but with the raw PG MIME wrap left IN to prove DecodeLine strips it
	return []byte(base64.StdEncoding.EncodeToString(nonce) + ":" + mimeWrap(base64.StdEncoding.EncodeToString(ct)) + ":" + name)
}

// T8: a >41-byte value sealed and dumped in PG's real MIME-wrapped base64 round-trips through
// decryptStdin (DecodeLine strips the line wraps, Open authenticates).
func TestDecryptStdin_MIMERoundtrip(t *testing.T) {
	box, err := sealbox.New(testKeyHex, "")
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	value := []byte("sk-or-v1-" + strings.Repeat("0123456789abcdef", 4)) // 73 chars -> ct b64 wraps at 76
	record := sealRecord(t, "openrouter-main", value)
	if !strings.Contains(string(record), "\n") {
		t.Fatal("test record has no MIME wrap — would not exercise the strip path")
	}
	got, err := decryptStdin(box, record)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Error("roundtrip mismatch through MIME-wrapped record")
	}
}

// COH2: a non-TTY stdout (pipe / docker json-file log) is refused — no plaintext written, non-zero exit.
func TestRunSecretDecrypt_RefusesNonTTY(t *testing.T) {
	t.Setenv(sealbox.EnvKey, testKeyHex)
	t.Setenv(sealbox.EnvKeyPrev, "")
	value := []byte("PLAINTEXTMARKER-do-not-leak")
	record := sealRecord(t, "leaky", value)

	var stdout, stderr bytes.Buffer
	rc := runSecretDecrypt(bytes.NewReader(record), &stdout, &stderr, false)
	if rc == 0 {
		t.Error("non-TTY stdout: exit 0, want non-zero")
	}
	if strings.Contains(stdout.String(), "PLAINTEXTMARKER") {
		t.Errorf("plaintext written to non-TTY stdout: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), "PLAINTEXTMARKER") {
		t.Errorf("plaintext leaked to stderr: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "non-TTY") {
		t.Errorf("refusal message unclear: %q", stderr.String())
	}
}

// With a TTY, the recovered plaintext is written to stdout (the emergency path works).
func TestRunSecretDecrypt_TTYRoundtrip(t *testing.T) {
	t.Setenv(sealbox.EnvKey, testKeyHex)
	t.Setenv(sealbox.EnvKeyPrev, "")
	value := []byte("recovered secret")
	record := sealRecord(t, "ok", value)

	var stdout, stderr bytes.Buffer
	rc := runSecretDecrypt(bytes.NewReader(record), &stdout, &stderr, true)
	if rc != 0 {
		t.Fatalf("exit %d, stderr=%q", rc, stderr.String())
	}
	if got := strings.TrimRight(stdout.String(), "\n"); got != string(value) {
		t.Errorf("stdout = %q, want %q", got, value)
	}
}

func TestRunSecretDecrypt_NoKeyFailsBeforeOutput(t *testing.T) {
	t.Setenv(sealbox.EnvKey, "")
	t.Setenv(sealbox.EnvKeyPrev, "")
	var stdout, stderr bytes.Buffer
	rc := runSecretDecrypt(bytes.NewReader([]byte("AAAA:BBBB:n")), &stdout, &stderr, true)
	if rc == 0 {
		t.Error("missing SECRETS_KEY: exit 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), sealbox.EnvKey) {
		t.Errorf("error should name %s: %q", sealbox.EnvKey, stderr.String())
	}
}
