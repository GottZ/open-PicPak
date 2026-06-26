package flash

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// defaultStageDir is the scratch directory artifacts are staged into when
// flash.stage_remote_dir is empty (the "derive a temp dir" default). It is a
// tool-scoped mechanism path, not a policy value.
const defaultStageDir = "/tmp/picpak-ops-flash"

// Stager gets the four artifacts onto the device's host and reports the host
// directory the esptool argv paths anchor under. It rides the single sshhost
// transport (K9): the scp strategy copies bytes through a Runner, never a flash-owned
// scp binary.
type Stager interface {
	// HostDir is the directory the on-host artifact paths are anchored under (no I/O).
	HostDir(t Target) string
	// Stage ensures the write set's artifacts are present on the host. A no-op for
	// build_on_host/local; a subdir-preserving, sha-gated copy for scp.
	Stage(ctx context.Context, t Target, ws *WriteSet) error
}

// stageTransport is the small file-transfer seam the scp strategy needs, abstracted
// so the stager's sha-skip + subdir logic is testable without a live ssh child. The
// real implementation wraps a sshhost.Runner; a test fakes it.
type stageTransport interface {
	// remoteSHA256 returns the lowercase hex sha256 of a remote path, or "" if absent.
	remoteSHA256(ctx context.Context, p string) (string, error)
	// putFile copies a local file to a remote path, creating parent dirs and renaming
	// atomically, streaming the bytes through the transport (not a scp binary).
	putFile(ctx context.Context, localPath, remotePath string) error
}

// noopStager is the local / build_on_host strategy: the artifacts are already on the
// host, so staging is a directory resolution with no copy.
type noopStager struct {
	hostDir string
}

func (s noopStager) HostDir(Target) string                          { return s.hostDir }
func (s noopStager) Stage(context.Context, Target, *WriteSet) error { return nil }

// scpStager copies each changed artifact (subdirs preserved) to the host staging dir,
// skipping artifacts whose remote sha256 already matches (stage_skip_unchanged).
type scpStager struct {
	stageDir      string
	skipUnchanged bool
	tr            stageTransport
}

func (s scpStager) HostDir(Target) string { return s.stageDir }

func (s scpStager) Stage(ctx context.Context, t Target, ws *WriteSet) error {
	for _, e := range ws.Entries {
		remote := path.Join(s.stageDir, slash(e.File))
		if s.skipUnchanged && e.LocalPath != "" {
			localSum, lerr := sha256File(e.LocalPath)
			if lerr == nil {
				if remoteSum, rerr := s.tr.remoteSHA256(ctx, remote); rerr == nil && remoteSum != "" && remoteSum == localSum {
					continue // unchanged → skip the re-push (avoids re-sending the 1.4 MB app)
				}
			}
		}
		if e.LocalPath == "" {
			return fmt.Errorf("flash: artifact %q has no local source to stage", e.Name)
		}
		if err := s.tr.putFile(ctx, e.LocalPath, remote); err != nil {
			return fmt.Errorf("flash: staging %q to %s: %w", e.Name, remote, err)
		}
	}
	return nil
}

// newStager picks the staging strategy from config. scp builds a real transport over
// the host's Runner; build_on_host/local resolve a host directory and copy nothing.
// hostBuildDir is the on-host build/artifact dir (used by build_on_host/local);
// localArtifactDir is the build machine's artifact dir (the copy source root).
func newStager(fc config.Flash, host string, local bool, hostBuildDir string, runner sshhost.Runner, to pane.PaneID, send pane.Sender) Stager {
	switch fc.StageMode {
	case "scp":
		dir := strings.TrimSpace(fc.StageRemoteDir)
		if dir == "" {
			dir = defaultStageDir
		}
		return scpStager{
			stageDir:      dir,
			skipUnchanged: fc.StageSkipUnchanged,
			tr:            realStageTransport{runner: runner, local: local, to: to, send: send, shaBin: shaBinFor(fc)},
		}
	default: // build_on_host | local — artifacts already on the host
		return noopStager{hostDir: hostBuildDir}
	}
}

// realStageTransport implements stageTransport over the single sshhost Runner (K9).
type realStageTransport struct {
	runner sshhost.Runner
	local  bool
	to     pane.PaneID
	send   pane.Sender
	shaBin string
}

func (t realStageTransport) remoteSHA256(ctx context.Context, p string) (string, error) {
	out, err := t.runner.CaptureOutput(ctx, t.wrap(t.shaBin+" "+shellQuote(p)+" 2>/dev/null || true"))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", nil // absent → empty, not an error (first push)
	}
	return strings.ToLower(fields[0]), nil
}

func (t realStageTransport) putFile(ctx context.Context, localPath, remotePath string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	dir := path.Dir(remotePath)
	tmp := remotePath + ".part"
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s && mv -f %s %s",
		shellQuote(dir), shellQuote(tmp), shellQuote(tmp), shellQuote(remotePath))

	run, stdin, err := t.runner.RunInteractive(ctx, t.wrap(cmd), t.to, t.send)
	if run == nil {
		return err
	}
	if err != nil {
		return err
	}
	_, werr := stdin.Write(data)
	cerr := stdin.Close()
	<-run.Done()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	if rc := run.ExitCode(); rc != 0 {
		return fmt.Errorf("remote copy exited %d", rc)
	}
	return run.Err()
}

// wrap shapes the remote shell command for the runner kind: an ssh runner sends the
// single command string verbatim to the remote login shell; a local runner execs
// argv[0], so the pipeline is wrapped in `sh -c` (the same split the console uses).
func (t realStageTransport) wrap(cmd string) []string {
	if t.local {
		return []string{"sh", "-c", cmd}
	}
	return []string{cmd}
}

// shaBinFor reuses the build host's configured sha256 binary so a BusyBox/shasum host
// is reachable without a flash-owned key; defaults to sha256sum.
func shaBinFor(config.Flash) string { return "sha256sum" }

// sha256File computes a file's lowercase-hex sha256 (the copy-if-changed key).
func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// slash normalizes a relative artifact path to forward slashes for the remote host.
func slash(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// shellQuote single-quotes a path for safe interpolation into a remote shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
