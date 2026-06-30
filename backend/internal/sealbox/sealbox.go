// Package sealbox seals secret values with AES-256-GCM before they land in the `secrets` table
// (migration 0008). Stdlib-only (crypto/aes, crypto/cipher) — deliberately NOT pgcrypto:
// pgp_sym_encrypt would ship the master key through the SQL wire protocol into
// pg_stat_statements / log_statement paths.
//
// The AAD binds every ciphertext to its row identity (the secret name), so a ciphertext copied onto
// another row fails authentication. A prev-key slot supports master-key rotation: Open falls back to
// SECRETS_KEY_PREV and reports usedPrev=true so the boot sweep can re-seal under the current key.
//
// Error paths forward cipher standard errors only ("message authentication failed") — never
// ciphertext, never key material, never plaintext fragments.
//
// Single-tenant: the secret name is the natural key and the whole AAD payload. A future multi-tenant
// revival re-adds a scope component (see migration 0008's TODO).
package sealbox

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// KeySize is the master-key length in bytes (AES-256).
const KeySize = 32

// aadPrefix versions the AAD layout. Bumping it (v2) would invalidate every stored ciphertext — it is
// part of the persisted format, not a constant to refactor freely.
const aadPrefix = "picpak:secret:v1:"

// EnvKey and EnvKeyPrev are the master-key environment variables. The values are 64 hex chars
// (32 bytes); generate with `openssl rand -hex 32`. Both are env-only by design: the key that unseals
// DB rows can never itself live in the DB.
const (
	EnvKey     = "SECRETS_KEY"
	EnvKeyPrev = "SECRETS_KEY_PREV"
)

// Box holds the AEAD instances derived from the current master key and the optional previous key
// (set only while a rotation sweep is in flight).
type Box struct {
	current cipher.AEAD
	prev    cipher.AEAD // nil unless SECRETS_KEY_PREV is set
}

// parseKey turns a 64-char hex master key into a ready GCM instance. The label names the offending
// env var in errors WITHOUT echoing the value.
func parseKey(label, hexKey string) (cipher.AEAD, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil {
		return nil, fmt.Errorf("sealbox: %s is not valid hex", label)
	}
	if len(raw) != KeySize {
		return nil, fmt.Errorf("sealbox: %s must be %d hex chars (%d bytes), got %d bytes", label, KeySize*2, KeySize, len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("sealbox: %s: %w", label, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("sealbox: %s: %w", label, err)
	}
	return gcm, nil
}

// New builds a Box from hex-encoded master keys. prevHex may be empty (no rotation in flight).
func New(currentHex, prevHex string) (*Box, error) {
	if strings.TrimSpace(currentHex) == "" {
		return nil, fmt.Errorf("sealbox: %s is empty", EnvKey)
	}
	current, err := parseKey(EnvKey, currentHex)
	if err != nil {
		return nil, err
	}
	b := &Box{current: current}
	if strings.TrimSpace(prevHex) != "" {
		prev, err := parseKey(EnvKeyPrev, prevHex)
		if err != nil {
			return nil, err
		}
		b.prev = prev
	}
	return b, nil
}

// FromEnv builds a Box from SECRETS_KEY / SECRETS_KEY_PREV. Used by the admin boot path and the
// -secret-decrypt break-glass mode (which reads ONLY env + stdin, no DB).
func FromEnv() (*Box, error) {
	return New(os.Getenv(EnvKey), os.Getenv(EnvKeyPrev))
}

// HasPrev reports whether a previous master key is loaded (rotation sweep pending).
func (b *Box) HasPrev() bool {
	return b.prev != nil
}

// aad builds the row-identity binding from the secret name. ValidSecretName guarantees name carries
// no ':' so the break-glass DecodeLine layout stays unambiguous.
func aad(name string) []byte {
	return []byte(aadPrefix + name)
}

// Seal encrypts plaintext bound to name. The nonce is 12 bytes, fresh from crypto/rand per call —
// sealing the same plaintext twice yields different nonces AND different ciphertexts.
func (b *Box) Seal(name string, plaintext []byte) (nonce, ciphertext []byte, err error) {
	if name == "" {
		return nil, nil, fmt.Errorf("sealbox: name is required")
	}
	if len(plaintext) == 0 {
		return nil, nil, fmt.Errorf("sealbox: plaintext is empty")
	}
	nonce = make([]byte, b.current.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("sealbox: nonce: %w", err)
	}
	ciphertext = b.current.Seal(nil, nonce, plaintext, aad(name))
	return nonce, ciphertext, nil
}

// Open decrypts a sealed secret bound to name. It tries the current key first, then the previous key
// (if loaded). usedPrev=true signals the caller (boot sweep) to re-seal with the current key. The
// returned error is the cipher standard error from the LAST attempt — it carries no ciphertext, no
// key material and no plaintext fragments.
func (b *Box) Open(name string, nonce, ciphertext []byte) (plaintext []byte, usedPrev bool, err error) {
	if name == "" {
		return nil, false, fmt.Errorf("sealbox: name is required")
	}
	ad := aad(name)
	plaintext, err = b.current.Open(nil, nonce, ciphertext, ad)
	if err == nil {
		return plaintext, false, nil
	}
	if b.prev != nil {
		var prevErr error
		plaintext, prevErr = b.prev.Open(nil, nonce, ciphertext, ad)
		if prevErr == nil {
			return plaintext, true, nil
		}
		err = prevErr
	}
	return nil, false, fmt.Errorf("sealbox: open %s: %w", name, err)
}

// DecodeLine parses the break-glass stdin format:
//
//	<nonce_b64>:<ct_b64>:<name>
//
// It consumes the FULL input (callers read stdin to EOF, never line-wise) and strips all CR/LF first:
// PostgreSQL's encode(bytea,'base64') is MIME (RFC 2045) and inserts a line break every 76 chars —
// every realistic secret value (>41 bytes plaintext = ct >57 bytes = b64 >76 chars) arrives
// multi-line, while short test values stay single-line and would mask the bug. A line-based reader
// sees only the first fragment and fails exactly in the break-glass emergency (negatively probed in
// the test). SplitN(…, 3) is exact: name cannot contain ':' (ValidSecretName), so three fields is the
// only valid shape.
func DecodeLine(input []byte) (nonce, ciphertext []byte, name string, err error) {
	clean := strings.NewReplacer("\r", "", "\n", "").Replace(string(bytes.TrimSpace(input)))
	if clean == "" {
		return nil, nil, "", fmt.Errorf("sealbox: empty input (want nonce_b64:ct_b64:name)")
	}
	parts := strings.SplitN(clean, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, nil, "", fmt.Errorf("sealbox: malformed input (want nonce_b64:ct_b64:name)")
	}
	nonce, err = base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, nil, "", fmt.Errorf("sealbox: nonce is not valid base64")
	}
	ciphertext, err = base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, nil, "", fmt.Errorf("sealbox: ciphertext is not valid base64")
	}
	return nonce, ciphertext, parts[2], nil
}
