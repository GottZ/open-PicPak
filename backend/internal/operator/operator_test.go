package operator

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeRow is a pgx.Row whose Scan either fills the destinations from a live row or reports
// pgx.ErrNoRows — enough to drive Authenticate without a DB.
type fakeRow struct {
	noRows  bool
	keyID   int64
	isAdmin bool
	label   string
	stored  []byte
}

func (r fakeRow) Scan(dest ...any) error {
	if r.noRows {
		return pgx.ErrNoRows
	}
	*(dest[0].(*int64)) = r.keyID
	*(dest[1].(*bool)) = r.isAdmin
	*(dest[2].(*string)) = r.label
	*(dest[3].(*[]byte)) = r.stored
	return nil
}

// fakeQuerier returns a canned row for QueryRow. Query/Exec are unused by Authenticate.
type fakeQuerier struct{ row fakeRow }

func (q fakeQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, nil
}
func (q fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return q.row }
func (q fakeQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

// countCompares swaps the package compare seam for one that tallies calls, restoring it on cleanup.
func countCompares(t *testing.T) *int {
	t.Helper()
	orig := compare
	var n int
	compare = func(a, b []byte) int { n++; return orig(a, b) }
	t.Cleanup(func() { compare = orig })
	return &n
}

// TestAuthenticate_MissRunsCompare is the operator timing structural probe (design §5 B2, W2 probe
// d): an UNKNOWN key (ErrNoRows) must still run a constant-time compare before returning — no early
// return before the compare. Red state: the old code returned at ErrNoRows with zero compares.
func TestAuthenticate_MissRunsCompare(t *testing.T) {
	n := countCompares(t)
	q := fakeQuerier{row: fakeRow{noRows: true}}

	res, ok, err := Authenticate(context.Background(), q, "some-unknown-token")
	if ok || err != nil || res != (AuthResult{}) {
		t.Fatalf("miss: want (zero,false,nil), got (%+v,%v,%v)", res, ok, err)
	}
	if *n != 1 {
		t.Fatalf("miss must run exactly one constant-time compare (no early return before it), got %d", *n)
	}
}

// TestAuthenticate_HitCompares: a live key whose stored hash matches the token authenticates, and
// the match runs through the constant-time compare (not a raw DB equality shortcut).
func TestAuthenticate_HitCompares(t *testing.T) {
	n := countCompares(t)
	tok := "live-operator-token"
	q := fakeQuerier{row: fakeRow{keyID: 7, isAdmin: true, label: "ci", stored: HashToken(tok)}}

	res, ok, err := Authenticate(context.Background(), q, tok)
	if !ok || err != nil {
		t.Fatalf("hit: want (result,true,nil), got (%+v,%v,%v)", res, ok, err)
	}
	if res.KeyID != 7 || !res.IsAdmin || res.Label != "ci" {
		t.Fatalf("hit: wrong identity %+v", res)
	}
	if *n != 1 {
		t.Fatalf("hit must run exactly one constant-time compare, got %d", *n)
	}
}

// TestAuthenticate_WrongStoredHash: a fetched row whose stored hash does not match the token fails
// the constant-time compare (defence-in-depth against a future non-hash lookup handle).
func TestAuthenticate_WrongStoredHash(t *testing.T) {
	n := countCompares(t)
	q := fakeQuerier{row: fakeRow{keyID: 9, stored: HashToken("a-different-token")}}

	if res, ok, err := Authenticate(context.Background(), q, "the-token"); ok || err != nil {
		t.Fatalf("mismatch: want (_,false,nil), got (%+v,%v,%v)", res, ok, err)
	}
	if *n != 1 {
		t.Fatalf("mismatch must run one compare, got %d", *n)
	}
}
