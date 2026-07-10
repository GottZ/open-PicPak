// Package argon2id hashes and verifies passwords with argon2id, encoding the parameters and salt
// into a self-describing PHC string ($argon2id$v=19$m=..,t=..,p=..$salt$hash). A stored hash
// therefore carries everything Verify needs, so the cost parameters can be retuned later without
// invalidating existing hashes. The compare is constant-time (crypto/subtle).
//
// This is the shared credential primitive behind admin_users password login (A28 W3) and Basic
// auth (delta-4): both verify against the same PHC column, so the hashing lives here once.
package argon2id

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Default cost parameters. ~64 MiB / verify (design delta-4: argon2id ≈ 50-100 ms + ~64 MiB RAM,
// public after W8). Encoded into every hash, so changing these only affects newly created hashes.
const (
	defTime    uint32 = 2
	defMemory  uint32 = 64 * 1024 // KiB -> 64 MiB
	defThreads uint8  = 1
	saltLen           = 16
	keyLen            = 32
)

var (
	// ErrInvalidHash is returned when a stored hash is not a well-formed argon2id PHC string.
	ErrInvalidHash = errors.New("argon2id: malformed PHC hash")
	// ErrIncompatibleVersion is returned when the PHC string encodes a different argon2 version.
	ErrIncompatibleVersion = errors.New("argon2id: incompatible argon2 version")
)

// Hash derives an argon2id PHC string from a plaintext password using a fresh random salt.
func Hash(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, defTime, defMemory, defThreads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, defMemory, defTime, defThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether password matches the argon2id PHC string phc. It re-derives the key with
// the parameters and salt read from phc and compares in constant time. A malformed phc is an error
// (false, err); a valid phc with a wrong password is (false, nil).
func Verify(password, phc string) (bool, error) {
	m, t, p, salt, want, err := decode(phc)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// VerifyDummy spends the same CPU as a real Verify against a fixed internal hash. Callers run it on
// the unknown-user path so that "no such user" costs the same wall-clock time as "wrong password",
// denying a username-enumeration timing oracle (design §4.3 / §5).
func VerifyDummy(password string) {
	dummyOnce.Do(func() { dummyHash, _ = Hash("\x00picpak-argon2id-dummy\x00") })
	_, _ = Verify(password, dummyHash)
}

// decode parses a $argon2id$v=19$m=..,t=..,p=..$salt$hash PHC string into its parameters, salt and
// expected key.
func decode(phc string) (m, t uint32, p uint8, salt, hash []byte, err error) {
	parts := strings.Split(phc, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", <salt>, <hash>]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	var version int
	if _, err = fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return 0, 0, 0, nil, nil, ErrIncompatibleVersion
	}
	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	if hash, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	return m, t, p, salt, hash, nil
}
