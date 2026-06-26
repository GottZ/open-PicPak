package console

import (
	"context"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// --- fakes ------------------------------------------------------------------

// fakeRunner is a sshhost.Runner whose RunInteractive hands back a no-op writer and a
// nil *Run, so a console session can be started and torn down without a real ssh
// child or any /dev access.
type fakeRunner struct {
	mu       sync.Mutex
	starts   int
	lastArgv []string
	wc       *fakeWriteCloser
}

func (f *fakeRunner) Argv(a []string) []string { return a }
func (f *fakeRunner) Run(context.Context, []string, pane.PaneID, pane.Sender) (*sshhost.Run, error) {
	return nil, nil
}
func (f *fakeRunner) CaptureOutput(context.Context, []string) (string, error) { return "", nil }
func (f *fakeRunner) CloseMaster() error                                      { return nil }
func (f *fakeRunner) RunInteractive(_ context.Context, argv []string, _ pane.PaneID, _ pane.Sender) (*sshhost.Run, io.WriteCloser, error) {
	f.mu.Lock()
	f.starts++
	f.lastArgv = argv
	wc := f.wc
	f.mu.Unlock()
	if wc == nil {
		wc = &fakeWriteCloser{}
	}
	return nil, wc, nil
}

type fakeWriteCloser struct {
	mu     sync.Mutex
	buf    []byte
	closed bool
}

func (w *fakeWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, errors.New("closed")
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}
func (w *fakeWriteCloser) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func newTestPane(t *testing.T) (*consolePane, *fakeRunner) {
	t.Helper()
	cfg, err := config.Load(config.Opts{})
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	base := pane.NewBase(pane.PaneID("console:test"), pane.KindConsole, context.Background(), func(tea.Msg) {})
	cp, ok := New(base, cfg).(*consolePane)
	if !ok {
		t.Fatal("New did not return *consolePane")
	}
	return cp, &fakeRunner{wc: &fakeWriteCloser{}}
}

// attach wires a fake runner + device and connects past the confirm guard.
func (cp *consolePane) attach(t *testing.T, fr *fakeRunner, port string) {
	t.Helper()
	cp.cc.ConfirmConnect = false
	cp.dev = deviceRef{Host: "host-stub", Port: port}
	cp.session = newSession(cp.Context(), fr, cp.cc, cp.dev, cp.ID(), cp.Sender())
	cp.connect(true)
	if !cp.session.Running() {
		t.Fatal("expected session running after connect")
	}
}

// --- ConnState transitions (table-driven) -----------------------------------

func TestConnStateTransitions(t *testing.T) {
	t.Run("onSilence only Attached→Quiet", func(t *testing.T) {
		cases := []struct {
			in, want ConnState
		}{
			{StateAttached, StateQuiet},
			{StateResetting, StateResetting},
			{StateQuiet, StateQuiet},
			{StateClosed, StateClosed},
			{StateSleeping, StateSleeping},
		}
		for _, c := range cases {
			if got := onSilence(c.in); got != c.want {
				t.Errorf("onSilence(%v) = %v, want %v", c.in, got, c.want)
			}
		}
	})

	t.Run("onData (re)attaches Resetting/Quiet", func(t *testing.T) {
		cases := []struct {
			in, want ConnState
		}{
			{StateResetting, StateAttached},
			{StateQuiet, StateAttached},
			{StateAttached, StateAttached},
			{StateClosed, StateClosed},
			{StateError, StateError},
		}
		for _, c := range cases {
			if got := onData(c.in); got != c.want {
				t.Errorf("onData(%v) = %v, want %v", c.in, got, c.want)
			}
		}
	})

	t.Run("onBanner forces Attached", func(t *testing.T) {
		for _, in := range []ConnState{StateIdle, StateResetting, StateQuiet, StateClosed} {
			if got := onBanner(in); got != StateAttached {
				t.Errorf("onBanner(%v) = %v, want attached", in, got)
			}
		}
	})

	t.Run("onExit classification", func(t *testing.T) {
		cases := []struct {
			op, execErr bool
			want        ConnState
		}{
			{true, false, StateClosed},    // operator close
			{true, true, StateClosed},     // operator wins over an exec err
			{false, true, StateError},     // exec/start failure
			{false, false, StateSleeping}, // genuine EOF / keep-awake loss
		}
		for _, c := range cases {
			if got := onExit(c.op, c.execErr); got != c.want {
				t.Errorf("onExit(op=%v,exec=%v) = %v, want %v", c.op, c.execErr, got, c.want)
			}
		}
	})
}

// Attached→Quiet on a STILL-OPEN silent stream (NOT Error, NOT a reconnect trigger).
func TestAttachedGoesQuietOnSilence(t *testing.T) {
	cp, fr := newTestPane(t)
	cp.cc.QuietTimeoutMS = 10
	cp.attach(t, fr, "DEVNODE")

	cp.applyData([]string{"=== PicPak Setup Console ==="})
	if cp.state != StateAttached {
		t.Fatalf("after banner data state = %v, want attached", cp.state)
	}
	// Force the silence window without sleeping in the test.
	cp.lastActivity = time.Now().Add(-time.Second).UnixNano()
	cp.onQuietTick()
	if cp.state != StateQuiet {
		t.Fatalf("after silence on a live stream state = %v, want quiet (not error/reconnect)", cp.state)
	}
}

// Exit classification through the model: operator close→Closed, EOF→Sleeping,
// exec error→Error.
func TestModelExitClassification(t *testing.T) {
	t.Run("operator close → Closed", func(t *testing.T) {
		cp, fr := newTestPane(t)
		cp.attach(t, fr, "DEVNODE")
		cp.session.Stop() // operator-initiated teardown
		cp.Update(sshhost.RunExitMsg{Code: 0, Err: context.Canceled})
		if cp.state != StateClosed {
			t.Fatalf("operator exit → %v, want closed", cp.state)
		}
	})

	t.Run("EOF / keep-awake loss → Sleeping", func(t *testing.T) {
		cp, fr := newTestPane(t)
		cp.cc.Reconnect = false // isolate the Sleeping classification from auto-reconnect
		cp.attach(t, fr, "DEVNODE")
		cp.Update(sshhost.RunExitMsg{Code: 0, Err: nil})
		if cp.state != StateSleeping {
			t.Fatalf("EOF exit → %v, want sleeping", cp.state)
		}
	})

	t.Run("exec failure → Error", func(t *testing.T) {
		cp, fr := newTestPane(t)
		cp.attach(t, fr, "DEVNODE")
		cp.Update(sshhost.RunExitMsg{Code: 1, Err: errors.New("ssh: connect failed")})
		if cp.state != StateError {
			t.Fatalf("exec error exit → %v, want error", cp.state)
		}
	})
}

// --- framing ----------------------------------------------------------------

func TestFrame(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"crlf", "a\r\nb\r\n", []string{"a", "b"}},
		{"bare cr", "a\rb", []string{"a", "b"}},
		{"lf", "a\nb", []string{"a", "b"}},
		{"unterminated prompt kept", "> ", []string{"> "}},
		{"empty", "", nil},
		{"mixed", "x\r\ny", []string{"x", "y"}},
		{"interior blank preserved", "a\n\nb", []string{"a", "", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := frame(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("frame(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestBannerMatchesFramedLine(t *testing.T) {
	cp, _ := newTestPane(t)
	// The firmware prints the banner wrapped in CRLF; after framing it lands on its
	// own line. Banner matching must succeed on that framed line, not be anchored
	// against the raw byte stream.
	matched := false
	for _, ln := range frame("\r\n=== PicPak Setup Console ===\r\n") {
		if cp.matchBanner(ln) {
			matched = true
		}
	}
	if !matched {
		t.Fatal("banner must match a framed line")
	}
	if cp.matchBanner("INFO output line") {
		t.Fatal("a non-banner line must not match")
	}
}

// --- transport fail-loud ----------------------------------------------------

func TestBuildPumpArgvFailLoud(t *testing.T) {
	cfg, err := config.Load(config.Opts{})
	if err != nil {
		t.Fatal(err)
	}
	cc := cfg.Console

	// Unresolved {port} MUST error and MUST NOT fall back to a literal device node.
	if _, err := buildPumpArgv(cc, transportSubst{Host: "h", Port: "", Baud: "115200"}, false); err == nil {
		t.Fatal("empty {port} must error (no literal fallback)")
	}

	// Resolved ssh: a single verbatim remote-command element, no leftover placeholder.
	argv, err := buildPumpArgv(cc, transportSubst{Host: "h", Port: "DEVNODE", Baud: "115200"}, false)
	if err != nil {
		t.Fatalf("resolved ssh argv: %v", err)
	}
	if len(argv) != 1 {
		t.Fatalf("ssh argv len = %d, want 1 (verbatim remote command)", len(argv))
	}
	if strings.Contains(argv[0], "{") {
		t.Fatalf("unresolved placeholder remains: %q", argv[0])
	}
	if !strings.Contains(argv[0], "DEVNODE") {
		t.Fatalf("{port} not substituted: %q", argv[0])
	}

	// Resolved local: shell-wrapped so the pipeline + trap run.
	largv, err := buildPumpArgv(cc, transportSubst{Port: "DEVNODE", Baud: "115200"}, true)
	if err != nil {
		t.Fatalf("resolved local argv: %v", err)
	}
	if len(largv) != 3 || largv[0] != localShell || largv[1] != "-c" {
		t.Fatalf("local wrap = %v, want [%s -c <cmd>]", largv, localShell)
	}

	// An unknown {token} left in the template MUST error (never silently shipped).
	if _, err := buildPumpArgv(config.Console{RemoteCommand: "echo {bogus}"}, transportSubst{Port: "DEVNODE"}, false); err == nil {
		t.Fatal("unknown placeholder must error")
	}
}

// --- K6: ReleaseDevicePortMsg honored ---------------------------------------

func TestReleaseDevicePortHonored(t *testing.T) {
	cp, fr := newTestPane(t)
	cp.attach(t, fr, "DEVNODE")

	// Non-matching port → ignored, session stays live.
	cp.Update(app.ReleaseDevicePortMsg{Port: "OTHERNODE"})
	if !cp.session.Running() {
		t.Fatal("non-matching release must be ignored")
	}

	// Matching port → session released so flash can claim the VID-gated port.
	cp.Update(app.ReleaseDevicePortMsg{Port: "DEVNODE"})
	if cp.session.Running() {
		t.Fatal("matching release must stop the session")
	}
	if cp.state != StateClosed {
		t.Fatalf("after release state = %v, want closed", cp.state)
	}
	if !cp.session.Closing() {
		t.Fatal("release teardown must be operator-class (Closing)")
	}
}

// --- catalog ----------------------------------------------------------------

func TestCatalogMirrorsFirmware(t *testing.T) {
	cmds := Catalog()
	if len(cmds) == 0 {
		t.Fatal("catalog is empty")
	}
	have := map[string]bool{}
	for _, c := range cmds {
		have[c.Name] = true
	}
	for _, name := range []string{"INFO", "NET", "OTA", "GUARD", "STORE", "LOG", "SETWIFI"} {
		if !have[name] {
			t.Errorf("catalog missing firmware command %s", name)
		}
	}
	if len(CommandNames()) != len(cmds) {
		t.Fatalf("CommandNames len %d != catalog len %d", len(CommandNames()), len(cmds))
	}
}

// --- air-gap: every console.* default is behavioral/templated ---------------

func TestConsoleDefaultsAreBehavioral(t *testing.T) {
	cfg, err := config.Load(config.Opts{})
	if err != nil {
		t.Fatal(err)
	}
	cc := cfg.Console

	ipv4 := regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	noDeviceNode := func(label, v string) {
		for _, node := range []string{"/dev/tty", "/dev/serial", "/dev/ttyACM", "/dev/ttyUSB"} {
			if strings.Contains(v, node) {
				t.Errorf("console default %s %q embeds a serial device node (%s)", label, v, node)
			}
		}
		if ipv4.MatchString(v) {
			t.Errorf("console default %s %q embeds an IP literal", label, v)
		}
	}

	// Transport/template fields are the endpoint-risk surface: no device node, no IP,
	// and no absolute path. /dev/null (a POSIX sink in the shell redirection) is
	// permitted — it touches no real device. The only values that touch a real
	// environment must arrive via the gated device/host record at runtime.
	endpoints := append([]string{cc.SSHCommand, cc.RemoteCommand, cc.BannerMatch, cc.SessionLogDir}, cc.SSHArgs...)
	for _, v := range endpoints {
		noDeviceNode("endpoint", v)
		if strings.HasPrefix(v, "/") {
			t.Errorf("console default endpoint %q is an absolute path literal", v)
		}
	}

	// Keybindings + enums are behavioral keyspecs (e.g. "/" is the search key, not a
	// path); they still must not smuggle a device node or IP.
	for _, v := range []string{
		cc.LineEndings,
		cc.Keys.Submit, cc.Keys.HistoryPrev, cc.Keys.HistoryNext, cc.Keys.ScrollUp, cc.Keys.ScrollDown,
		cc.Keys.Search, cc.Keys.Clear, cc.Keys.Reconnect, cc.Keys.Disconnect, cc.Keys.Help,
	} {
		noDeviceNode("keyspec", v)
	}
}
