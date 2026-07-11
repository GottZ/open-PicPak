package main

import (
	"fmt"
	"io"
	"os"

	"github.com/open-picpak/backend/internal/sealbox"
)

// stdoutIsTTY reports whether stdout is an interactive terminal (a character device). Secret-grade
// plaintext — bootstrap tokens, break-glass values — is printed ONLY to a TTY, never to a captured
// non-TTY stdout that Docker's json-file driver or journald would persist as plaintext-at-rest (COH2).
func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// decryptStdin is the break-glass core: an env-keyed Box + one stdin record (nonce_b64:ct_b64:name) →
// recovered plaintext. No DB, no TTY — separated so the decode/open correctness and the TTY gate test
// independently.
func decryptStdin(box *sealbox.Box, input []byte) ([]byte, error) {
	nonce, ct, name, err := sealbox.DecodeLine(input)
	if err != nil {
		return nil, err
	}
	plaintext, _, err := box.Open(name, nonce, ct)
	return plaintext, err
}

// runSecretDecrypt is the `admin -secret-decrypt` subcommand: SECRETS_KEY[_PREV] from env, one record
// from stdin, NO DB — so it works under `docker run` even while the admin service crash-loops (the
// emergency itself). COH2: the recovered plaintext is written ONLY to an interactive TTY; a captured
// non-TTY stdout (a pipe, a docker json-file log) is refused with a non-zero exit so a recovered secret
// never persists as plaintext-at-rest.
func runSecretDecrypt(stdin io.Reader, stdout, stderr io.Writer, isTTY bool) int {
	box, err := sealbox.FromEnv()
	if err != nil {
		fmt.Fprintf(stderr, "secret-decrypt: %v\n", err)
		return 1
	}
	input, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "secret-decrypt: read stdin: %v\n", err)
		return 1
	}
	if !isTTY {
		fmt.Fprintln(stderr, "secret-decrypt: refusing to write plaintext to a non-TTY stdout "+
			"(it would persist in container logs); run it attached to an interactive terminal")
		return 3
	}
	plaintext, err := decryptStdin(box, input)
	if err != nil {
		fmt.Fprintf(stderr, "secret-decrypt: %v\n", err)
		return 1
	}
	_, _ = stdout.Write(append(plaintext, '\n'))
	return 0
}
