package ingestcore

// C2 long-poll wakeup hub (Design 16). A single Postgres LISTEN connection receives the
// command_queue insert NOTIFY (migration 0006) and broadcasts it in-process to every waiting C2
// long-poll handler. This keeps command-arrival signalling at ONE database connection for the whole
// fleet, instead of one held connection per polling device.

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// c2Notifier is a close-and-replace broadcast: wait() hands out the current channel, broadcast()
// closes it (waking every waiter) and installs a fresh one. A waiter grabs the channel BEFORE
// re-querying, so an insert racing between the query and the select still wakes it (no lost wakeup).
type c2Notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func newC2Notifier() *c2Notifier { return &c2Notifier{ch: make(chan struct{})} }

// wait returns a channel closed on the next broadcast. Re-query the command queue after it closes.
func (n *c2Notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *c2Notifier) broadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	close(n.ch)
	n.ch = make(chan struct{})
}

// listenLoop holds a dedicated pool connection on LISTEN c2_cmd and broadcasts every notification,
// reconnecting on error. Runs for the process lifetime. One DB connection total.
func (n *c2Notifier) listenLoop(ctx context.Context, pool *pgxpool.Pool) {
	for ctx.Err() == nil {
		if err := n.listenOnce(ctx, pool); err != nil && ctx.Err() == nil {
			log.Printf("c2 listen: %v (reconnect in 1s)", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

func (n *c2Notifier) listenOnce(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN c2_cmd"); err != nil {
		return err
	}
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		n.broadcast()
	}
}
