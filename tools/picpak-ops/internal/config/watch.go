package config

import (
	"context"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/fsnotify/fsnotify"
)

// ReloadMsg is emitted by the watch Cmd when the loaded config file changes. On
// Err==nil the shell swaps its *Config pointer; on Err!=nil it surfaces a
// non-fatal toast and keeps the last-good config (a bad edit never tears down
// running panes).
//
// K4: config emits ReloadMsg{Config, Err} only. It does NOT define
// ConfigChangedMsg — that is wm-shell's taxonomy, a later wave.
type ReloadMsg struct {
	Config *Config
	Err    error
}

// WatchCmd returns a tea.Cmd that wraps the fsnotify watch into the Bubble Tea
// loop: it blocks on the next reload event and returns one ReloadMsg, then the
// shell re-issues it. The blocking receive selects on ctx.Done() so on quit
// (rootCtx cancelled) the watch goroutine returns and the re-issued Cmd
// terminates with a nil msg (no goroutine leak past p.Run()).
//
// opts carries the same resolution inputs Load used, so a reload re-runs the
// full load pipeline (strict decode → env → overrides → validate → freeze) and
// emits a fresh *Config.
func WatchCmd(ctx context.Context, path string, opts Opts) tea.Cmd {
	if path == "" || ctx == nil {
		return nil
	}
	if !opts.RequireFile {
		// A reload always targets the same resolved file, so pin it.
		opts.Path = path
	} else {
		opts.Path = path
	}

	ch := watch(ctx, path, opts)
	var listen tea.Cmd
	listen = func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			return msg
		}
	}
	return listen
}

// watch starts a debounced fsnotify watcher on path's parent directory (so
// atomic-rename editors are caught) and emits a ReloadMsg per coalesced change.
// The channel is closed when ctx is cancelled or the watcher fails to start.
func watch(ctx context.Context, path string, opts Opts) <-chan ReloadMsg {
	out := make(chan ReloadMsg, 1)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		out <- ReloadMsg{Err: err}
		close(out)
		return out
	}

	// Watch the parent dir: editors that write via temp-file + rename do not fire
	// a write event on the target inode, only on the directory.
	dir := filepath.Dir(path)
	target := filepath.Clean(path)
	if err := w.Add(dir); err != nil {
		out <- ReloadMsg{Err: err}
		_ = w.Close()
		close(out)
		return out
	}

	debounce := opts.debounceOr(500 * time.Millisecond)

	go func() {
		defer close(out)
		defer func() { _ = w.Close() }()

		var timer *time.Timer
		var timerC <-chan time.Time
		arm := func() {
			if timer == nil {
				timer = time.NewTimer(debounce)
			} else {
				timer.Reset(debounce)
			}
			timerC = timer.C
		}

		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Clean(ev.Name) != target {
					continue
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
					continue
				}
				arm()
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				emit(ctx, out, ReloadMsg{Err: err})
			case <-timerC:
				timerC = nil
				cfg, lerr := Load(opts)
				emit(ctx, out, ReloadMsg{Config: cfg, Err: lerr})
			}
		}
	}()

	return out
}

// emit sends a ReloadMsg without blocking past ctx cancellation.
func emit(ctx context.Context, out chan<- ReloadMsg, msg ReloadMsg) {
	select {
	case <-ctx.Done():
	case out <- msg:
	}
}

// debounceOr returns the configured reload debounce, or the fallback if zero.
// The watcher cannot see the freshly-loaded reload.debounce (chicken-and-egg on
// the very first arm), so the caller passes the loaded value via Opts when
// available; otherwise the default fallback applies.
func (o Opts) debounceOr(fallback time.Duration) time.Duration {
	if o.ReloadDebounce > 0 {
		return o.ReloadDebounce
	}
	return fallback
}
