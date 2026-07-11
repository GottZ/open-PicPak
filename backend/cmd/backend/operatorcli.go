package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/operator"
)

// newOperatorToken mints a fresh bearer (32 random bytes, URL-safe base64 so it rides an Authorization
// header cleanly) and its sha256 — the only form persisted. The plaintext is returned to the caller to
// print ONCE and is never written anywhere.
func newOperatorToken() (token string, hash []byte) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("crypto/rand failed: " + err.Error()) // a CSPRNG failure must not silently weaken a key
	}
	token = base64.RawURLEncoding.EncodeToString(raw[:])
	return token, operator.HashToken(token)
}

// runOperatorCLI dispatches the operator-key subcommands. Returns a process exit code.
func runOperatorCLI(cmd string, args []string) int {
	ctx := context.Background()
	pool, err := openPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db: %v\n", err)
		return 1
	}
	defer pool.Close()

	switch cmd {
	case "create-operator":
		fs := flag.NewFlagSet("create-operator", flag.ContinueOnError)
		label := fs.String("label", "", "human label for the key (required)")
		isAdmin := fs.Bool("admin", false, "grant admin (mutation) privilege")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *label == "" {
			fmt.Fprintln(os.Stderr, "create-operator: -label is required")
			return 2
		}
		return createOperator(ctx, pool, *label, *isAdmin)
	case "list-operators":
		return listOperators(ctx, pool)
	case "disable-operator":
		fs := flag.NewFlagSet("disable-operator", flag.ContinueOnError)
		id := fs.Int64("id", 0, "operator key id to revoke (required)")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *id <= 0 {
			fmt.Fprintln(os.Stderr, "disable-operator: -id is required")
			return 2
		}
		return disableOperator(ctx, pool, *id)
	case "rotate-operator":
		fs := flag.NewFlagSet("rotate-operator", flag.ContinueOnError)
		id := fs.Int64("id", 0, "operator key id to rotate (required)")
		grace := fs.Duration("expires-in", 24*time.Hour, "grace window before the OLD key expires (e.g. 24h, 30m)")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *id <= 0 {
			fmt.Fprintln(os.Stderr, "rotate-operator: -id is required")
			return 2
		}
		if *grace < 0 {
			fmt.Fprintln(os.Stderr, "rotate-operator: -expires-in must not be negative")
			return 2
		}
		return rotateOperator(ctx, pool, *id, *grace)
	}
	return 2
}

func createOperator(ctx context.Context, pool *pgxpool.Pool, label string, isAdmin bool) int {
	// COH2: the minted bearer is shown once and must never land in a captured non-TTY stdout that
	// Docker/journald persists as plaintext-at-rest. Refuse BEFORE minting/inserting so a non-TTY run
	// leaves no orphan key whose token nobody ever saw — run it attached to an interactive terminal.
	if !stdoutIsTTY() {
		fmt.Fprintln(os.Stderr, "create-operator: refusing to print a bearer token to a non-TTY stdout "+
			"(it would persist in container logs); run it attached to an interactive terminal")
		return 3
	}
	token, hash := newOperatorToken()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO operator_keys (token_hash, label, is_admin) VALUES ($1, $2, $3) RETURNING id`,
		hash, label, isAdmin).Scan(&id); err != nil {
		fmt.Fprintf(os.Stderr, "create-operator: %v\n", err)
		return 1
	}
	role := "read-only"
	if isAdmin {
		role = "ADMIN"
	}
	fmt.Printf("operator key #%d created  (label=%q  role=%s)\n", id, label, role)
	fmt.Printf("token (shown once — store it now, it cannot be recovered):\n  %s\n", token)
	return 0
}

func listOperators(ctx context.Context, pool *pgxpool.Pool) int {
	// token_hash is NEVER selected — there is no read path for key material.
	rows, err := pool.Query(ctx,
		`SELECT id, label, is_admin, created_at, last_used_at, disabled_at
		   FROM operator_keys ORDER BY id`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list-operators: %v\n", err)
		return 1
	}
	defer rows.Close()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLABEL\tROLE\tCREATED\tLAST USED\tSTATUS")
	for rows.Next() {
		var (
			id                   int64
			label                string
			isAdmin              bool
			created              time.Time
			lastUsed, disabledAt *time.Time
		)
		if err := rows.Scan(&id, &label, &isAdmin, &created, &lastUsed, &disabledAt); err != nil {
			fmt.Fprintf(os.Stderr, "list-operators: %v\n", err)
			return 1
		}
		role := "read-only"
		if isAdmin {
			role = "admin"
		}
		status := "active"
		if disabledAt != nil {
			status = "revoked " + disabledAt.Format("2006-01-02")
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n",
			id, label, role, created.Format("2006-01-02"), fmtTime(lastUsed), status)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "list-operators: %v\n", err)
		return 1
	}
	_ = tw.Flush()
	return 0
}

func disableOperator(ctx context.Context, pool *pgxpool.Pool, id int64) int {
	// Soft revoke: a disabled key authenticates as absent (operator.Authenticate filters disabled_at
	// IS NULL), so this is effective on the key's very next request (SEC-M1). Idempotent: only flips a
	// still-live key, so re-running reports "already revoked or unknown".
	tag, err := pool.Exec(ctx,
		`UPDATE operator_keys SET disabled_at = now() WHERE id = $1 AND disabled_at IS NULL`, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "disable-operator: %v\n", err)
		return 1
	}
	if tag.RowsAffected() == 0 {
		// nothing flipped: distinguish "unknown id" from "already revoked" for the operator.
		var existed bool
		err := pool.QueryRow(ctx, `SELECT true FROM operator_keys WHERE id = $1`, id).Scan(&existed)
		if errors.Is(err, pgx.ErrNoRows) {
			fmt.Fprintf(os.Stderr, "disable-operator: no key with id %d\n", id)
			return 1
		}
		fmt.Printf("operator key #%d is already revoked\n", id)
		return 0
	}
	fmt.Printf("operator key #%d revoked (rejected on its next request)\n", id)
	return 0
}

// rotation is the result of a soft operator-key rotation: a freshly minted replacement plus the
// grace expiry stamped on the retired key.
type rotation struct {
	OldID     int64
	NewID     int64
	Token     string
	Label     string
	IsAdmin   bool
	OldExpiry time.Time
}

// rotateOperatorKey performs the DB side of a soft rotation (design §5 B8 / W7), separate from the
// TTY-guarded CLI wrapper so the once-shown-token discipline is not in the tested path. It mints a
// replacement carrying the SAME label + is_admin as the key being rotated (an equivalent-capability
// successor), and stamps expires_at = now()+grace on the OLD key — a soft retirement on a window,
// NOT a hard disable, so an in-flight client keeps working until the grace elapses (then the W7
// expiry check in operator.Authenticate rejects it). Mint + stamp run in ONE transaction so a crash
// never leaves the old key retired without a live replacement, nor an orphan replacement. The old
// key must exist and be live (a revoked key is a mint, not a rotation).
func rotateOperatorKey(ctx context.Context, pool *pgxpool.Pool, oldID int64, grace time.Duration) (rotation, error) {
	var (
		label      string
		isAdmin    bool
		disabledAt *time.Time
	)
	err := pool.QueryRow(ctx,
		`SELECT label, is_admin, disabled_at FROM operator_keys WHERE id = $1`, oldID).
		Scan(&label, &isAdmin, &disabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return rotation{}, fmt.Errorf("no operator key with id %d", oldID)
	}
	if err != nil {
		return rotation{}, err
	}
	if disabledAt != nil {
		return rotation{}, fmt.Errorf("operator key #%d is revoked; mint a fresh one with create-operator", oldID)
	}

	token, hash := newOperatorToken()
	oldExpiry := time.Now().Add(grace)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return rotation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit

	var newID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO operator_keys (token_hash, label, is_admin) VALUES ($1, $2, $3) RETURNING id`,
		hash, label, isAdmin).Scan(&newID); err != nil {
		return rotation{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE operator_keys SET expires_at = $1 WHERE id = $2`, oldExpiry, oldID); err != nil {
		return rotation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return rotation{}, err
	}
	return rotation{OldID: oldID, NewID: newID, Token: token, Label: label, IsAdmin: isAdmin, OldExpiry: oldExpiry}, nil
}

func rotateOperator(ctx context.Context, pool *pgxpool.Pool, oldID int64, grace time.Duration) int {
	// COH2: as with create-operator, refuse to print a bearer to a non-TTY stdout BEFORE any mint or
	// insert — a captured stdout would persist the replacement token as plaintext-at-rest, and the
	// guard-before-write keeps a non-TTY run from retiring the old key with no token anyone ever saw.
	if !stdoutIsTTY() {
		fmt.Fprintln(os.Stderr, "rotate-operator: refusing to print a bearer token to a non-TTY stdout "+
			"(it would persist in container logs); run it attached to an interactive terminal")
		return 3
	}
	rot, err := rotateOperatorKey(ctx, pool, oldID, grace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rotate-operator: %v\n", err)
		return 1
	}
	role := "read-only"
	if rot.IsAdmin {
		role = "ADMIN"
	}
	fmt.Printf("operator key #%d rotated → new key #%d  (label=%q  role=%s)\n", rot.OldID, rot.NewID, rot.Label, role)
	fmt.Printf("old key #%d stays valid until %s (grace window), then authenticates as absent\n",
		rot.OldID, rot.OldExpiry.Format(time.RFC3339))
	fmt.Printf("token (shown once — store it now, it cannot be recovered):\n  %s\n", rot.Token)
	return 0
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Format("2006-01-02 15:04")
}
