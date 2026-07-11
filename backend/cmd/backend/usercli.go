package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"golang.org/x/term"

	"github.com/open-picpak/backend/internal/adminuser"
)

// runUserCLI dispatches the admin_users bootstrap subcommand. It is TTY-only like create-operator, but
// for the mirror-image reason: it READS a password with no echo, never from argv (which would land in
// shell history and the process table as plaintext). There is no chicken-and-egg — the CLI needs no
// login, so it mints the first admin_user before any HTTP session can exist (design §3.2 backfill).
//
//	admin create-user -username <name> [-admin]   # prompt for a password (no echo), create the account
func runUserCLI(cmd string, args []string) int {
	ctx := context.Background()
	switch cmd {
	case "create-user":
		fs := flag.NewFlagSet("create-user", flag.ContinueOnError)
		username := fs.String("username", "", "login username (required)")
		isAdmin := fs.Bool("admin", false, "grant admin (fleet-mutation) privilege")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *username == "" {
			fmt.Fprintln(os.Stderr, "create-user: -username is required")
			return 2
		}
		return createUser(ctx, *username, *isAdmin)
	}
	return 2
}

func createUser(ctx context.Context, username string, isAdmin bool) int {
	// The password must be read interactively with no echo. Refuse a non-TTY stdin so it can never be
	// taken from a pipe/heredoc that a shell history or a CI log would persist as plaintext-at-rest —
	// the read-side analogue of create-operator's write-side TTY guard (S11).
	if !stdinIsTTY() {
		fmt.Fprintln(os.Stderr, "create-user: refusing to read a password from a non-TTY stdin "+
			"(it would persist in shell history / logs); run it attached to an interactive terminal")
		return 3
	}
	pw, err := promptPassword("password: ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create-user: %v\n", err)
		return 1
	}
	if pw == "" {
		fmt.Fprintln(os.Stderr, "create-user: empty password")
		return 2
	}
	confirm, err := promptPassword("confirm password: ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create-user: %v\n", err)
		return 1
	}
	if pw != confirm {
		fmt.Fprintln(os.Stderr, "create-user: passwords do not match")
		return 2
	}

	pool, err := openPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db: %v\n", err)
		return 1
	}
	defer pool.Close()

	u, err := adminuser.Create(ctx, pool, username, pw, isAdmin)
	if errors.Is(err, adminuser.ErrUsernameTaken) {
		fmt.Fprintf(os.Stderr, "create-user: username %q already exists\n", username)
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "create-user: %v\n", err)
		return 1
	}
	role := "read-only"
	if isAdmin {
		role = "ADMIN"
	}
	fmt.Printf("admin user #%d created  (username=%q  role=%s)\n", u.ID, username, role)
	return 0
}

// promptPassword reads one line from the terminal with echo disabled.
func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// stdinIsTTY reports whether stdin is an interactive terminal. It uses term.IsTerminal (a real termios
// ioctl), NOT the os.ModeCharDevice heuristic: /dev/null is a character device but not a terminal, so
// the heuristic would false-positive and let a `create-user < /dev/null` slip past the no-echo guard.
func stdinIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
