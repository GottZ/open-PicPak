package commandstore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Script size is validated before any DB access (a nil pool proves it): empty and over-cap are
// rejected up front. Red: an unchecked script reaches the queue (an empty no-op, or a RAM-spiking blob).
func TestEnqueue_ScriptValidation(t *testing.T) {
	if _, err := Enqueue(context.Background(), nil, "*", "", nil, 1); !errors.Is(err, ErrScriptEmpty) {
		t.Fatalf("empty: want ErrScriptEmpty, got %v", err)
	}
	if _, err := Enqueue(context.Background(), nil, "*", strings.Repeat("x", ScriptMax+1), nil, 1); !errors.Is(err, ErrScriptTooLong) {
		t.Fatalf("over-cap: want ErrScriptTooLong, got %v", err)
	}
}
