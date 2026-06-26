package flash

import (
	"context"
	"os"
	"path/filepath"
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

// mustFlash loads the compiled defaults and returns the [flash] view + the four
// build.artifacts (the offset→file truth).
func mustFlash(t *testing.T) (config.Flash, []config.BuildArtifact) {
	t.Helper()
	cfg, err := config.Load(config.Opts{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("config.Load defaults: %v", err)
	}
	return cfg.Flash, cfg.Build.Artifacts
}

// scrambledArtifacts returns the four artifacts in a NON-sorted order, so BuildWriteSet
// must sort them ascending (proving the app-last property is the sort, not luck).
func scrambledArtifacts() []config.BuildArtifact {
	return []config.BuildArtifact{
		{Name: "app", File: "picpak_fw.bin", Offset: "0x20000"},
		{Name: "bootloader", File: "bootloader/bootloader.bin", Offset: "0x0"},
		{Name: "otadata", File: "ota_data_initial.bin", Offset: "0x10000"},
		{Name: "partitions", File: "partition_table/partition-table.bin", Offset: "0x8000"},
	}
}

// TestValidateWriteSet_RejectsNVSAndErase is THE gate (the inviolable NVS-preserve
// property proved by FIRST breaking it — methodology red→green). It asserts:
//   - an injected 0x9000 (NVS) target is REJECTED;
//   - a size-overlap into NVS is REJECTED;
//   - an erase-flash and an erase-region verb (dashed AND underscored) are REJECTED;
//   - the legitimate 4-artifact write set is ACCEPTED — and is offset-sorted with the
//     app LAST.
func TestValidateWriteSet_RejectsNVSAndErase(t *testing.T) {
	fc, _ := mustFlash(t)
	guard := GuardFromConfig(fc)
	if guard.Start != 0x9000 || guard.Len != 0x6000 {
		t.Fatalf("guard from config = %#x/%#x, want 0x9000/0x6000", guard.Start, guard.Len)
	}

	// Build the legit set from scrambled artifacts; files absent → size 0.
	ws, err := BuildWriteSet(t.TempDir(), scrambledArtifacts())
	if err != nil {
		t.Fatalf("BuildWriteSet: %v", err)
	}

	// ACCEPTED: legit argv + write set passes the guard.
	port := "DEVNODE"
	argv := BuildEsptoolArgv(fc, port, "/stage", ws)
	if err := ValidateWriteSet(ws, argv, guard); err != nil {
		t.Fatalf("legit write set must be accepted, got: %v", err)
	}

	// ...and is offset-sorted with the app LAST (the abort-safety property).
	wantOrder := []uint64{0x0, 0x8000, 0x10000, 0x20000}
	if len(ws.Entries) != 4 {
		t.Fatalf("write set has %d entries, want 4", len(ws.Entries))
	}
	for i, e := range ws.Entries {
		if e.Offset != wantOrder[i] {
			t.Fatalf("entry %d offset = %#x, want %#x (not offset-sorted)", i, e.Offset, wantOrder[i])
		}
	}
	if last := ws.Entries[len(ws.Entries)-1]; last.Name != "app" || last.Offset != 0x20000 {
		t.Fatalf("last entry = %q@%#x, want app@0x20000 (app must be written LAST)", last.Name, last.Offset)
	}

	// REJECTED: an injected 0x9000 (NVS) target.
	nvs := &WriteSet{Entries: append(append([]WriteEntry(nil), ws.Entries...),
		WriteEntry{Name: "evil-nvs", Offset: 0x9000, Size: 16, File: "evil.bin"})}
	if err := ValidateWriteSet(nvs, BuildEsptoolArgv(fc, port, "/stage", nvs), guard); err == nil {
		t.Fatal("an injected 0x9000 (NVS) target MUST be rejected (factory NVS preserve)")
	} else if !strings.Contains(err.Error(), "NVS") {
		t.Fatalf("0x9000 rejection error should name NVS, got: %v", err)
	}

	// REJECTED: a bootloader whose SIZE overlaps into NVS (the real-world trigger).
	big := &WriteSet{Entries: []WriteEntry{{Name: "bootloader", Offset: 0x0, Size: 0x9001, File: "bootloader/bootloader.bin"}}}
	if err := ValidateWriteSet(big, BuildEsptoolArgv(fc, port, "/stage", big), guard); err == nil {
		t.Fatal("a write whose [offset,offset+size) reaches into NVS MUST be rejected")
	}

	// REJECTED: erase verbs in the argv, dashed AND underscored.
	for _, verb := range []string{"erase-flash", "erase_flash", "erase-region", "erase_region"} {
		eraseArgv := append([]string{"esptool", verb}, argv...)
		if err := ValidateWriteSet(ws, eraseArgv, guard); err == nil {
			t.Fatalf("erase verb %q in argv MUST be rejected (fail-closed)", verb)
		}
	}
}

// TestController_ValidateFailClosed_NoRun proves the safety property GATES the
// transport: an artifact at 0x9000 makes validateWriteSet fail-closed and the
// controller performs NO HostRegistry.Run (no "run:" event), surfacing FailValidate.
func TestController_ValidateFailClosed_NoRun(t *testing.T) {
	fc, arts := mustFlash(t)
	evil := append(append([]config.BuildArtifact(nil), arts...),
		config.BuildArtifact{Name: "evil", File: "evil.bin", Offset: "0x9000"})

	rec := newRecorder()
	ft := newFakeTransport(rec, successRing(len(evil)), 0)
	ctrl := NewController(context.Background(), fc, evil, t.TempDir(), ft,
		noopStagerFor, pane.PaneID("flash:1"), rec.send)

	ctrl.Start([]Target{NewTarget(sshhost.DiscoveredDevice{Host: "host-a", TTY: "DEVNODE"})})
	rec.waitBatch(t)

	if ft.ranAny() {
		t.Fatal("validate must fail-closed: NO transport.Run on an NVS-overlapping write set")
	}
	done := rec.deviceDones()
	if len(done) != 1 || done[0].Class != FailValidate {
		t.Fatalf("want one FailValidate device-done, got %+v", done)
	}
}

// TestController_EmitsReleaseBeforeRun_AndConfirms proves K6 + the gate: the controller
// calls HostRegistry.Confirm before the run, EMITS ReleaseDevicePortMsg before the run,
// and reports a PER-DEVICE matrix (never a single batch bool).
func TestController_EmitsReleaseBeforeRun_AndConfirms(t *testing.T) {
	fc, arts := mustFlash(t)
	rec := newRecorder()
	ft := newFakeTransport(rec, successRing(len(arts)), 0)
	ctrl := NewController(context.Background(), fc, arts, t.TempDir(), ft,
		noopStagerFor, pane.PaneID("flash:1"), rec.send)

	targets := []Target{
		NewTarget(sshhost.DiscoveredDevice{Host: "host-a", TTY: "ACM0"}),
		NewTarget(sshhost.DiscoveredDevice{Host: "host-b", TTY: "ACM1"}),
	}
	ctrl.Start(targets)
	rec.waitBatch(t)

	// Confirm was called for both devices.
	if got := ft.confirmedTTYs(); len(got) != 2 {
		t.Fatalf("Confirm called for %v, want both devices", got)
	}

	// For each target: Confirm BEFORE Run, and a ReleaseDevicePortMsg seen BEFORE Run.
	for _, host := range []string{"host-a", "host-b"} {
		if !ft.confirmBeforeRun(host) {
			t.Fatalf("%s: Confirm did not precede Run", host)
		}
		if !ft.releaseSeenBeforeRun(host) {
			t.Fatalf("%s: ReleaseDevicePortMsg (K6) was not emitted before the run", host)
		}
	}
	// The release ports match the device tty nodes.
	if !rec.hasReleasePort("ACM0") || !rec.hasReleasePort("ACM1") {
		t.Fatalf("ReleaseDevicePortMsg missing a device port: %v", rec.releasePorts())
	}

	// PER-DEVICE matrix: two distinct device-done results + a per-device summary map,
	// never a single collapsed batch bool.
	if dones := rec.deviceDones(); len(dones) != 2 {
		t.Fatalf("want 2 per-device done messages, got %d", len(dones))
	}
	batch := rec.batchSummary(t)
	if len(batch) != 2 {
		t.Fatalf("batch summary has %d entries, want one per device (matrix must not collapse)", len(batch))
	}
	for id, ok := range batch {
		if !ok {
			t.Fatalf("device %q not OK on a success ring", id)
		}
	}
}

// --- fakes ------------------------------------------------------------------

// noopStagerFor is the test stager: no host copy, a fixed host dir.
func noopStagerFor(Target) (Stager, error) { return noopStager{hostDir: "/stage"}, nil }

// successRing is an esptool output ring that hash-verifies n files and exits clean.
func successRing(n int) []string {
	lines := []string{"esptool v9.0.0", "Chip is ESP32-C3", "Flash will be erased? no"}
	for i := 0; i < n; i++ {
		lines = append(lines,
			"Writing at 0x00000000... (100 %)",
			"Wrote 21152 bytes at 0x00000000 in 0.4 seconds...",
			"Hash of data verified.")
	}
	return append(lines, "Leaving...", "Hard resetting via RTS pin...")
}

// fakeRun is a RunHandle whose output ring is pre-filled and whose Done is closed.
type fakeRun struct {
	id   sshhost.RunID
	ring *pane.Scrollback
	done chan struct{}
	code int
}

func (r *fakeRun) ID() sshhost.RunID      { return r.id }
func (r *fakeRun) Ring() *pane.Scrollback { return r.ring }
func (r *fakeRun) Done() <-chan struct{}  { return r.done }
func (r *fakeRun) ExitCode() int          { return r.code }
func (r *fakeRun) Err() error             { return nil }
func (r *fakeRun) Cancel()                {}

// fakeTransport records Confirm + Run calls (with ordering) so the controller's
// sequence is asserted without a live ssh child.
type fakeTransport struct {
	mu        sync.Mutex
	rec       *recorder
	ringLines []string
	exit      int

	confirmed        []string        // tty per Confirm
	events           []string        // "confirm:<host>" / "run:<host>" in call order
	releaseBeforeRun map[string]bool // host → a release was already emitted when Run was called
}

func newFakeTransport(rec *recorder, ring []string, exit int) *fakeTransport {
	return &fakeTransport{rec: rec, ringLines: ring, exit: exit, releaseBeforeRun: map[string]bool{}}
}

func (t *fakeTransport) Confirm(dev sshhost.DiscoveredDevice) error {
	t.mu.Lock()
	t.confirmed = append(t.confirmed, dev.TTY)
	t.events = append(t.events, "confirm:"+dev.Host)
	t.mu.Unlock()
	return nil
}

func (t *fakeTransport) Run(ctx context.Context, host string, argv []string, to pane.PaneID) (RunHandle, error) {
	port := portFromArgv(argv)
	seen := t.rec.hasReleasePort(port)
	t.mu.Lock()
	t.events = append(t.events, "run:"+host)
	t.releaseBeforeRun[host] = seen
	t.mu.Unlock()

	done := make(chan struct{})
	close(done)
	ring := pane.NewScrollback(200)
	for _, l := range t.ringLines {
		ring.Append(l)
	}
	return &fakeRun{id: sshhost.RunID("run-" + host), ring: ring, done: done, code: t.exit}, nil
}

func (t *fakeTransport) ranAny() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.events {
		if strings.HasPrefix(e, "run:") {
			return true
		}
	}
	return false
}

func (t *fakeTransport) confirmedTTYs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.confirmed...)
}

func (t *fakeTransport) confirmBeforeRun(host string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ci, ri := -1, -1
	for i, e := range t.events {
		if e == "confirm:"+host && ci < 0 {
			ci = i
		}
		if e == "run:"+host && ri < 0 {
			ri = i
		}
	}
	return ci >= 0 && ri >= 0 && ci < ri
}

func (t *fakeTransport) releaseSeenBeforeRun(host string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.releaseBeforeRun[host]
}

// portFromArgv extracts the value after "-p" (the device node esptool writes to).
func portFromArgv(argv []string) string {
	for i, a := range argv {
		if a == "-p" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// recorder is the Sender sink: it records every emitted message and signals when the
// terminal FlashBatchDoneMsg arrives.
type recorder struct {
	mu    sync.Mutex
	msgs  []tea.Msg
	batch chan struct{}
	once  sync.Once
}

func newRecorder() *recorder { return &recorder{batch: make(chan struct{})} }

func (r *recorder) send(msg tea.Msg) {
	r.mu.Lock()
	r.msgs = append(r.msgs, msg)
	r.mu.Unlock()
	if pm, ok := msg.(app.PaneMsg); ok {
		if _, ok := pm.Payload.(FlashBatchDoneMsg); ok {
			r.once.Do(func() { close(r.batch) })
		}
	}
}

func (r *recorder) waitBatch(t *testing.T) {
	t.Helper()
	select {
	case <-r.batch:
	case <-time.After(5 * time.Second):
		t.Fatal("no FlashBatchDoneMsg within 5s")
	}
}

func (r *recorder) hasReleasePort(port string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msgs {
		if rp, ok := m.(app.ReleaseDevicePortMsg); ok && rp.Port == port {
			return true
		}
	}
	return false
}

func (r *recorder) releasePorts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.msgs {
		if rp, ok := m.(app.ReleaseDevicePortMsg); ok {
			out = append(out, rp.Port)
		}
	}
	return out
}

func (r *recorder) deviceDones() []FlashDeviceDoneMsg {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []FlashDeviceDoneMsg
	for _, m := range r.msgs {
		if pm, ok := m.(app.PaneMsg); ok {
			if d, ok := pm.Payload.(FlashDeviceDoneMsg); ok {
				out = append(out, d)
			}
		}
	}
	return out
}

func (r *recorder) batchSummary(t *testing.T) map[TargetID]bool {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msgs {
		if pm, ok := m.(app.PaneMsg); ok {
			if b, ok := pm.Payload.(FlashBatchDoneMsg); ok {
				return b.Summary
			}
		}
	}
	t.Fatal("no FlashBatchDoneMsg recorded")
	return nil
}

// --- staging ----------------------------------------------------------------

// fakeStageTr records putFile copies and returns canned remote shas (the copy-if-
// changed seam) without a live ssh child.
type fakeStageTr struct {
	remoteSums map[string]string
	puts       []string
}

func (f *fakeStageTr) remoteSHA256(_ context.Context, p string) (string, error) {
	return f.remoteSums[p], nil
}

func (f *fakeStageTr) putFile(_ context.Context, _, remote string) error {
	f.puts = append(f.puts, remote)
	return nil
}

// TestScpStager_SkipsUnchanged_ReproducesSubdirs proves staging copies changed
// artifacts (subdirs preserved) and SKIPS one whose remote sha already matches.
func TestScpStager_SkipsUnchanged_ReproducesSubdirs(t *testing.T) {
	dir := t.TempDir()
	// A local artifact with a subdir path and known content.
	bootDir := filepath.Join(dir, "bootloader")
	if err := os.MkdirAll(bootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bootLocal := filepath.Join(bootDir, "bootloader.bin")
	appLocal := filepath.Join(dir, "picpak_fw.bin")
	if err := os.WriteFile(bootLocal, []byte("BOOT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appLocal, []byte("APP-PAYLOAD"), 0o644); err != nil {
		t.Fatal(err)
	}
	bootSum, _ := sha256File(bootLocal)

	ws := &WriteSet{Entries: []WriteEntry{
		{Name: "bootloader", Offset: 0x0, File: "bootloader/bootloader.bin", LocalPath: bootLocal},
		{Name: "app", Offset: 0x20000, File: "picpak_fw.bin", LocalPath: appLocal},
	}}

	fake := &fakeStageTr{remoteSums: map[string]string{
		"/stage/bootloader/bootloader.bin": bootSum, // already up to date → skipped
	}}
	s := scpStager{stageDir: "/stage", skipUnchanged: true, tr: fake}

	if err := s.Stage(context.Background(), Target{Host: "host-a"}, ws); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if len(fake.puts) != 1 || fake.puts[0] != "/stage/picpak_fw.bin" {
		t.Fatalf("expected only the changed app copied to /stage/picpak_fw.bin, got %v", fake.puts)
	}
	if s.HostDir(Target{}) != "/stage" {
		t.Fatalf("HostDir = %q, want /stage", s.HostDir(Target{}))
	}
}
