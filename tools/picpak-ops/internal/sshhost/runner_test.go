package sshhost

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// fakeSSHEnv, when set, makes the test binary impersonate `ssh` instead of running
// tests (the self-exec fake-ssh trick): the binary is already executable, so this
// works even where the temp dir is mounted noexec and a shell-script shim cannot run.
const fakeSSHEnv = "PICPAK_FAKE_SSH"

// TestMain dispatches into the fake-ssh impersonation when fakeSSHEnv is set;
// otherwise it runs the package tests normally.
func TestMain(md *testing.M) {
	if os.Getenv(fakeSSHEnv) != "" {
		os.Exit(fakeSSHMain())
	}
	os.Exit(md.Run())
}

// fakeSSHMain impersonates `ssh`: it echoes each received arg as ARG:<arg>, prints
// READY, then blocks until killed (so a test can prove Cancel terminates the child).
func fakeSSHMain() int {
	for _, a := range os.Args[1:] {
		fmt.Printf("ARG:%s\n", a)
	}
	fmt.Println("READY")
	// Block "forever"; the test cancels the context which kills this process.
	select {}
}

// --- argv assembly (table) ---------------------------------------------------

func TestSSHRunnerArgv(t *testing.T) {
	rc := runConfig{
		sshBinary:   "ssh",
		baseArgs:    []string{"-o", "BatchMode=yes", "-o", "ControlMaster=auto"},
		controlPath: "/home/op/.ssh/cm-picpak-%r@%h:%p",
	}
	cases := []struct {
		name   string
		target string
		argv   []string
		want   []string
	}{
		{
			name:   "poll probe with control path",
			target: "host-a",
			argv:   []string{"ls", "-l", "/dev/serial/by-id/"},
			want: []string{
				"ssh", "-o", "BatchMode=yes", "-o", "ControlMaster=auto",
				"-o", "ControlPath=/home/op/.ssh/cm-picpak-%r@%h:%p",
				"host-a", "--", "ls", "-l", "/dev/serial/by-id/",
			},
		},
		{
			name:   "remote argv after the separator is not interpreted by ssh",
			target: "user@host.example",
			argv:   []string{"udevadm", "info", "-q", "property", "-n", "/dev/ttyACM0"},
			want: []string{
				"ssh", "-o", "BatchMode=yes", "-o", "ControlMaster=auto",
				"-o", "ControlPath=/home/op/.ssh/cm-picpak-%r@%h:%p",
				"user@host.example", "--",
				"udevadm", "info", "-q", "property", "-n", "/dev/ttyACM0",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newSSHRunner(rc, tc.target)
			got := r.Argv(tc.argv)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Argv()\n got = %v\nwant = %v", got, tc.want)
			}
		})
	}
}

func TestSSHRunnerArgvNoControlPath(t *testing.T) {
	rc := runConfig{sshBinary: "ssh", baseArgs: []string{"-o", "BatchMode=yes"}}
	r := newSSHRunner(rc, "host-a")
	got := r.Argv([]string{"echo", "hi"})
	want := []string{"ssh", "-o", "BatchMode=yes", "host-a", "--", "echo", "hi"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Argv() with no control path\n got = %v\nwant = %v", got, want)
	}
}

func TestLocalRunnerArgvVerbatim(t *testing.T) {
	r := newLocalRunner(runConfig{})
	got := r.Argv([]string{"ls", "-l", "/dev/serial/by-id/"})
	want := []string{"ls", "-l", "/dev/serial/by-id/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("local Argv()\n got = %v\nwant = %v", got, want)
	}
}

// --- control_path ~-expansion ------------------------------------------------

func TestExpandHome(t *testing.T) {
	home := homeDir()
	if home == "" {
		t.Skip("no home dir available")
	}
	cases := map[string]string{
		"":                  "",
		"/abs/path":         "/abs/path",
		"~":                 home,
		"~/.ssh/cm-%r@%h":   filepath.Join(home, ".ssh/cm-%r@%h"),
		"relative/no/tilde": "relative/no/tilde",
	}
	for in, want := range cases {
		if got := expandHome(in); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- fake-ssh shim: argv passthrough + cancellation --------------------------

// collector is a goroutine-safe Sender sink that records RunOutputMsg lines and the
// RunExitMsg, so a test can observe the stream without a tea.Program.
type collector struct {
	mu    sync.Mutex
	lines []string
	exit  *RunExitMsg
	ready chan struct{}
	once  sync.Once
}

func newCollector() *collector { return &collector{ready: make(chan struct{})} }

func (c *collector) send(msg tea.Msg) {
	pm, ok := msg.(app.PaneMsg)
	if !ok {
		return
	}
	switch p := pm.Payload.(type) {
	case RunOutputMsg:
		c.mu.Lock()
		c.lines = append(c.lines, p.Line)
		c.mu.Unlock()
		if p.Line == "READY" {
			c.once.Do(func() { close(c.ready) })
		}
	case RunExitMsg:
		c.mu.Lock()
		c.exit = &p
		c.mu.Unlock()
	}
}

func (c *collector) snapshot() ([]string, *RunExitMsg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...), c.exit
}

func TestSSHRunnerRunAndCancel(t *testing.T) {
	// Impersonate ssh via the test binary itself (self-exec), so the test works even
	// where temp dirs are mounted noexec and a shell-script shim cannot run.
	t.Setenv(fakeSSHEnv, "1")

	rc := runConfig{
		sshBinary:   os.Args[0], // the running test binary acts as `ssh` (see TestMain)
		baseArgs:    []string{"-o", "BatchMode=yes"},
		controlPath: "/run/cm-%r@%h:%p",
		runScroll:   100,
		lineMax:     8192,
	}
	r := newSSHRunner(rc, "host-a")

	col := newCollector()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	run, err := r.Run(ctx, []string{"ls", "/dev/serial/by-id/"}, pane.PaneID("hosts:1"), col.send)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Wait for the shim's READY line (proves the stream is flowing).
	select {
	case <-col.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for fake ssh to stream READY")
	}

	// The shim echoed each arg: assert the runner passed our assembled argv,
	// including the remote command after the "--" separator.
	lines, _ := col.snapshot()
	wantArg := func(s string) bool {
		for _, l := range lines {
			if l == "ARG:"+s {
				return true
			}
		}
		return false
	}
	for _, expect := range []string{"-o", "BatchMode=yes", "host-a", "--", "ls", "/dev/serial/by-id/"} {
		if !wantArg(expect) {
			t.Errorf("fake ssh did not receive expected arg %q; got lines=%v", expect, lines)
		}
	}

	// Cancel: the child (sleeping 30s) must be killed and a terminal RunExitMsg
	// must arrive promptly — proving ctx-cancel teardown (§9).
	run.Cancel()
	select {
	case <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel did not terminate the run within 5s")
	}
	_, exit := col.snapshot()
	if exit == nil {
		t.Fatal("no RunExitMsg after Cancel")
	}
	if exit.RunID != run.ID() {
		t.Fatalf("RunExitMsg.RunID = %q, want %q", exit.RunID, run.ID())
	}
}

func TestLocalRunnerCaptureOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/echo")
	}
	r := newLocalRunner(runConfig{runScroll: 10, lineMax: 4096})
	out, err := r.CaptureOutput(context.Background(), []string{"echo", "hello-picpak"})
	if err != nil {
		t.Fatalf("CaptureOutput: %v", err)
	}
	if got := trimTrailingNL(out); got != "hello-picpak" {
		t.Fatalf("CaptureOutput = %q, want %q", got, "hello-picpak")
	}
}

func TestCaptureOutputNonZeroCarriesStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	r := newLocalRunner(runConfig{})
	_, err := r.CaptureOutput(context.Background(),
		[]string{"sh", "-c", "echo boom 1>&2; exit 3"})
	if err == nil {
		t.Fatal("expected an error for a non-zero exit")
	}
	if got := err.Error(); !contains(got, "boom") {
		t.Fatalf("error %q does not carry the stderr tail", got)
	}
}

func TestLocalRunnerEmptyArgv(t *testing.T) {
	r := newLocalRunner(runConfig{})
	if _, err := r.Run(context.Background(), nil, "x", func(tea.Msg) {}); err == nil {
		t.Fatal("empty argv should error")
	}
}

func trimTrailingNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
