package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/open-picpak/backend/internal/adminaudit"
	"github.com/open-picpak/backend/internal/apitoken"
)

// runTokenCLI dispatches the api-token bootstrap subcommand. It mints the first machine token before any
// HTTP admin exists (no chicken-and-egg — the CLI needs no login), the CLI analogue of the HTTP mint in
// token_http.go (design §4.5).
//
//	admin create-api-token -label <name> [-scopes image:read,image:write] [-expires-in 720h]
func runTokenCLI(cmd string, args []string) int {
	ctx := context.Background()
	switch cmd {
	case "create-api-token":
		fs := flag.NewFlagSet("create-api-token", flag.ContinueOnError)
		label := fs.String("label", "", "human label for the token (required)")
		scopes := fs.String("scopes", "image:read,image:write", "comma-separated scopes")
		expiresIn := fs.Duration("expires-in", 0, "expiry from now (e.g. 720h); 0 = no expiry")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *label == "" {
			fmt.Fprintln(os.Stderr, "create-api-token: -label is required")
			return 2
		}
		return createApiToken(ctx, *label, splitScopes(*scopes), *expiresIn)
	}
	return 2
}

func createApiToken(ctx context.Context, label string, scopes []string, expiresIn time.Duration) int {
	// COH2 / S11: the minted token is shown once and must never land in a captured non-TTY stdout that
	// Docker/journald persists as plaintext-at-rest. Refuse BEFORE opening the pool or minting so a
	// non-TTY run leaves no orphan token whose plaintext nobody ever saw — run it attached to a terminal.
	if !stdoutIsTTY() {
		fmt.Fprintln(os.Stderr, "create-api-token: refusing to print a bearer token to a non-TTY stdout "+
			"(it would persist in container logs); run it attached to an interactive terminal")
		return 3
	}

	pool, err := openPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db: %v\n", err)
		return 1
	}
	defer pool.Close()

	var expiresAt *time.Time
	if expiresIn > 0 {
		t := time.Now().Add(expiresIn)
		expiresAt = &t
	}
	// createdBy = nil: a CLI-bootstrapped token is attributed to no operator (design §4.5); the audit
	// row below records actor_kind='cli'.
	plaintext, tok, err := apitoken.Create(ctx, pool, label, scopes, expiresAt, nil)
	if errors.Is(err, apitoken.ErrUnknownScope) {
		fmt.Fprintf(os.Stderr, "create-api-token: %v (known scopes: image:read, image:write)\n", err)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "create-api-token: %v\n", err)
		return 1
	}

	// Synchronous audit trail entry, mirroring the HTTP mint (design §4.6): actor_kind='cli', target is
	// the token_id (never the plaintext secret).
	_ = adminaudit.Write(ctx, pool, adminaudit.Entry{
		ActorKind: "cli", Action: "token.mint", Target: tok.TokenID, OK: true,
	})

	fmt.Printf("api token #%d created  (label=%q  scopes=%s%s)\n",
		tok.ID, label, strings.Join(tok.Scopes, ","), expiryNote(tok.ExpiresAt))
	fmt.Printf("token (shown once — store it now, it cannot be recovered):\n  %s\n", plaintext)
	return 0
}

// splitScopes turns the comma-separated -scopes flag into a trimmed, empty-dropped slice. Validation of
// the values themselves is apitoken.Create's job (ErrUnknownScope).
func splitScopes(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func expiryNote(t *time.Time) string {
	if t == nil {
		return "  no-expiry"
	}
	return "  expires=" + t.Format(time.RFC3339)
}
