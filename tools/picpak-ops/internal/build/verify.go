package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// lowerHex64 is the exact contract OTA/firmware-register depend on: 64 lowercase hex
// digits. ota.c compares the firmware digest via strcmp (NOT strcasecmp) and the
// backend CHECK is ^[0-9a-f]{64}$, so an uppercased hash silently fail-closes OTA on
// every device. Go's hex.EncodeToString emits lowercase; this asserts it never
// regressed.
var lowerHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// fileProbe abstracts the host-side reads verify needs so the same logic serves a
// local build (os/crypto) and a remote one (ssh stat/sha256sum/cat). Local reads run
// in-process; remote reads are bounded ssh subprocesses sharing the run context.
type fileProbe interface {
	size(path string) (int64, error)
	sha256hex(path string) (string, error)
	readFile(path string) ([]byte, error)
}

// flasherArgs is the subset of build/flasher_args.json verify cross-checks: the
// per-build offset→file truth IDF emits, plus the chip.
type flasherArgs struct {
	FlashFiles map[string]string `json:"flash_files"`
	Extra      struct {
		Chip string `json:"chip"`
	} `json:"extra_esptool_args"`
}

// Verify is the regression gate's evidence (§2.2): stat the configured artifacts,
// parse flasher_args.json and cross-check every offset→file mapping (after path-base
// normalization so a build/ prefix difference is not a false alarm), bound each
// size, confirm the app fits ota_0, and compute the app's lowercase-hex SHA-256. It
// returns the verified ArtifactSet (nil when any blocking finding fired) plus all
// findings (blocking and warning). It never panics and never touches a device.
func Verify(cfg Config, probe fileProbe) (*ArtifactSet, []Finding) {
	var findings []Finding
	block := func(format string, a ...any) {
		findings = append(findings, Finding{Stage: StageVerify, Severity: SeverityBlock, Text: fmt.Sprintf(format, a...)})
	}
	warn := func(format string, a ...any) {
		findings = append(findings, Finding{Stage: StageVerify, Severity: SeverityWarn, Text: fmt.Sprintf(format, a...)})
	}

	set := &ArtifactSet{}
	sizeByName := map[string]int64{}

	// 1. Stat each artifact + bound its size.
	for _, a := range cfg.Artifacts {
		sz, err := probe.size(a.PathAbs)
		if err != nil {
			block("missing artifact %q at %s", a.Name, a.PathAbs)
			continue
		}
		sizeByName[a.Name] = sz
		if a.ExactSize > 0 && sz != a.ExactSize {
			block("artifact %q size %d B != expected %d B", a.Name, sz, a.ExactSize)
		}
		if a.MinSize > 0 && sz < a.MinSize {
			block("artifact %q size %d B is below the %d B lower bound", a.Name, sz, a.MinSize)
		}
		set.Artifacts = append(set.Artifacts, Artifact{
			Name: a.Name, File: a.File, Path: a.PathAbs, Offset: a.Offset, Size: sz,
		})
	}

	// 2. Parse flasher_args.json and cross-check offset→file against the config.
	if raw, err := probe.readFile(cfg.FlasherArgsPath); err != nil {
		block("cannot read flasher_args.json at %s", cfg.FlasherArgsPath)
	} else {
		var fa flasherArgs
		if err := json.Unmarshal(raw, &fa); err != nil {
			block("flasher_args.json is not valid JSON: %v", err)
		} else {
			// Re-key flash_files by numeric offset so "0x0"/"0x00" compare equal.
			byOffset := map[uint64]string{}
			for off, file := range fa.FlashFiles {
				if n, err := strconv.ParseUint(strings.TrimSpace(off), 0, 64); err == nil {
					byOffset[n] = file
				}
			}
			for _, a := range cfg.Artifacts {
				want, err := strconv.ParseUint(strings.TrimSpace(a.Offset), 0, 64)
				if err != nil {
					block("artifact %q has an unparseable offset %q", a.Name, a.Offset)
					continue
				}
				got, ok := byOffset[want]
				if !ok {
					block("flasher_args.json has no entry for offset %s (artifact %q)", a.Offset, a.Name)
					continue
				}
				if normalizeBuildPath(got) != normalizeBuildPath(a.File) {
					block("offset %s maps to %q in flasher_args.json but config expects %q (artifact %q)",
						a.Offset, got, a.File, a.Name)
				}
			}
			if cfg.Target != "" && fa.Extra.Chip != "" && fa.Extra.Chip != cfg.Target {
				warn("flasher_args.json chip %q != configured target %q", fa.Extra.Chip, cfg.Target)
			}
		}
	}

	// 3. App-specific checks: fits ota_0, lowercase-hex SHA-256.
	appSize, err := probe.size(cfg.AppPath)
	if err != nil {
		block("missing app artifact at %s", cfg.AppPath)
	} else {
		if cfg.OTASlotSize > 0 && appSize >= cfg.OTASlotSize {
			block("app size %d B does not fit the ota_0 slot (%d B)", appSize, cfg.OTASlotSize)
		}
		sum, err := probe.sha256hex(cfg.AppPath)
		if err != nil {
			block("cannot hash the app artifact at %s: %v", cfg.AppPath, err)
		} else if !lowerHex64.MatchString(sum) {
			block("app sha256 %q is not 64 lowercase hex digits (OTA compares via strcmp)", sum)
		} else {
			set.AppSHA256 = sum
		}
		set.App = Artifact{Name: "app", Path: cfg.AppPath, Size: appSize}
		for _, a := range set.Artifacts {
			if a.Name == "app" {
				set.App = a
				break
			}
		}
	}

	// 4. Opportunistic firmware version (firmware/version.txt) — not a gate.
	if raw, err := probe.readFile(resolveUnder(cfg.FirmwareDir, "version.txt")); err == nil {
		set.Version = strings.TrimSpace(string(raw))
	}

	if hasBlocking(findings) {
		return nil, findings
	}
	return set, findings
}

// hasBlocking reports whether any finding aborts the build.
func hasBlocking(fs []Finding) bool {
	for _, f := range fs {
		if f.Severity == SeverityBlock {
			return true
		}
	}
	return false
}

// normalizeBuildPath reconciles the two path bases the offset cross-check compares:
// flasher_args.json values are relative to firmware_dir/build/ (no build/ prefix),
// while a config path may carry one. It slash-normalizes, cleans, and strips a single
// leading build/ so a prefix difference alone is never a false-alarm (§2.1).
func normalizeBuildPath(p string) string {
	p = path.Clean(strings.ReplaceAll(p, "\\", "/"))
	p = strings.TrimPrefix(p, "build/")
	return p
}

// localFileProbe reads artifacts on this box with os/crypto, no subprocess (§2.2).
type localFileProbe struct{}

func (localFileProbe) size(p string) (int64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func (localFileProbe) sha256hex(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil // lowercase by construction
}

func (localFileProbe) readFile(p string) ([]byte, error) { return os.ReadFile(p) }

// remoteFileProbe reads artifacts on a remote build host through the sshhost Runner
// (bounded ssh stat/sha256sum/cat sharing the run context). The binaries are config
// (stat_bin/sha256_bin) so a BusyBox or shasum host is reachable without a code edit.
type remoteFileProbe struct {
	ctx     context.Context
	runner  sshhost.Runner
	statBin string
	shaBin  string
}

func (r remoteFileProbe) size(p string) (int64, error) {
	out, err := r.runner.CaptureOutput(r.ctx, []string{r.statBin, "-c", "%s", p})
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

func (r remoteFileProbe) sha256hex(p string) (string, error) {
	out, err := r.runner.CaptureOutput(r.ctx, []string{r.shaBin, p})
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("empty sha256 output for %s", p)
	}
	return strings.ToLower(fields[0]), nil
}

func (r remoteFileProbe) readFile(p string) ([]byte, error) {
	out, err := r.runner.CaptureOutput(r.ctx, []string{"cat", p})
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// newFileProbe picks the probe for the host kind: local os/crypto, or a remote ssh
// probe bound to the run context.
func newFileProbe(ctx context.Context, cfg Config, runner sshhost.Runner) fileProbe {
	if cfg.Local {
		return localFileProbe{}
	}
	return remoteFileProbe{ctx: ctx, runner: runner, statBin: cfg.StatBin, shaBin: cfg.Sha256Bin}
}
