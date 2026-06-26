// Package build is the firmware build pipeline (W5 / axis 04): an ordered,
// cancelable set of stages — preflight → codegen(×4) → docker-build → verify → done
// — that turns the open-picpak firmware tree into a verified, flashable artifact set
// without the operator touching a shell. It owns no transport of its own: every
// command runs through the single sshhost.Runner (K9). The pipeline is host-agnostic
// (build.host = "local" or a [[hosts]] id); the only local≠remote split is in verify
// (local stats artifacts with os/crypto, remote shells out to stat/sha256sum).
//
// "Build green" is a regression gate, not a correctness/display proof — the verify
// result the pane renders carries that disclaimer explicitly (HW empiricism).
package build

import (
	"path/filepath"
	"time"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// Config is the typed, resolved view of the [build] section the pipeline runs
// against. Paths are pre-resolved to their on-host form once at construction so no
// stage re-derives them: relative firmware/artifact paths anchor on repo_path (made
// absolute for a local host so the result is independent of the process cwd; left
// as-configured for a remote host, where firmware_dir should be absolute).
type Config struct {
	Host  string // build.host id ("local" or a [[hosts]].name)
	Local bool   // host == "local"/"" → local executor (verify uses os/crypto)

	RepoAbs     string // repo root used for the docker {repo} bind mount (absolute for local)
	FirmwareDir string // firmware tree on the host (cwd anchor; absolute for local)
	ArtifactDir string // build output dir (absolute for local)

	DockerCmd         string
	DockerImage       string
	Target            string
	BuildCmd          []string
	DockerRunArgs     []string
	ContainerNameTmpl string

	PythonBin         string
	PythonImportProbe string
	StatBin           string
	Sha256Bin         string
	FontsDir          string // resolved against FirmwareDir

	Setup              []SetupStep
	Codegen            []CodegenSpec
	RequiredComponents []string // components/<c> dirs (resolved against FirmwareDir)

	Artifacts       []ArtifactSpec
	FlasherArgsPath string // resolved against FirmwareDir
	AppPath         string // app binary to hash (resolved against FirmwareDir)
	OTASlotSize     int64

	ErrorTailLines  int
	PaneScrollback  int
	GateDisclaimer  string
	CancelKillGrace time.Duration

	CancelKey string
	RerunKey  string
}

// SetupStep is one resolved one-time component-setup script (offered by preflight on
// confirm). Argv runs the script by its absolute path (the scripts resolve their own
// paths from $0, so no cwd is needed).
type SetupStep struct {
	Name string
	Argv []string
}

// CodegenSpec is one resolved host generator. Argv = [interpreter, absolute script,
// extra args…]; the generators write their headers via __file__, so cwd is not
// needed. Output/Src are firmware-relative display/preflight paths; SrcAbs is the
// resolved Berry source a render/policy generator reads (empty for screens/font16).
type CodegenSpec struct {
	Name   string
	Argv   []string
	Output string // generated header (firmware-relative, for the failure label)
	Src    string // Berry source read by the generator (firmware-relative)
	SrcAbs string // resolved Src ("" when none)
}

// ArtifactSpec is one resolved expected flash artifact. PathAbs is the on-host file;
// File is the build-dir-relative path that the flasher_args.json offset→file cross-
// check compares against (both sides are relative to the build dir, §2.1).
type ArtifactSpec struct {
	Name      string
	File      string // relative to the build (artifact) dir — matches flasher_args flash_files values
	PathAbs   string
	Offset    string // hex, as configured
	MinSize   int64  // size must be >= this (0 = unset)
	ExactSize int64  // size must equal this (0 = unset)
}

// NewConfig resolves the [build] section into a runnable Config. repoOverride, when
// non-empty, replaces build.repo_path (the tests point it at the real firmware tree);
// it does not mutate the shared *config.Config.
func NewConfig(cfg *config.Config, repoOverride string) Config {
	b := cfg.Build

	repoPath := b.RepoPath
	if repoOverride != "" {
		repoPath = repoOverride
	}
	local := b.Host == "" || b.Host == "local"

	// For a local build, anchor on an absolute repo root so verify (os.Stat) and the
	// docker -v mount work regardless of the process cwd. For a remote build, leave
	// the operator's (absolute) paths untouched.
	repoBase := repoPath
	if local {
		if abs, err := filepath.Abs(repoPath); err == nil {
			repoBase = abs
		}
	}

	firmwareDir := resolveUnder(repoBase, b.FirmwareDir)
	artifactDir := resolveUnder(repoBase, b.ArtifactDir)

	out := Config{
		Host:               b.Host,
		Local:              local,
		RepoAbs:            repoBase,
		FirmwareDir:        firmwareDir,
		ArtifactDir:        artifactDir,
		DockerCmd:          b.DockerCmd,
		DockerImage:        b.DockerImage,
		Target:             b.Target,
		BuildCmd:           append([]string(nil), b.BuildCmd...),
		DockerRunArgs:      append([]string(nil), b.DockerRunArgs...),
		ContainerNameTmpl:  b.ContainerNameTmpl,
		PythonBin:          b.PythonBin,
		PythonImportProbe:  b.PythonImportProbe,
		StatBin:            b.StatBin,
		Sha256Bin:          b.Sha256Bin,
		FontsDir:           resolveUnder(firmwareDir, b.FontsDir),
		RequiredComponents: append([]string(nil), b.RequiredComponents...),
		FlasherArgsPath:    resolveUnder(firmwareDir, b.FlasherArgsPath),
		AppPath:            resolveUnder(firmwareDir, b.AppArtifact),
		OTASlotSize:        b.OTASlotSize,
		ErrorTailLines:     b.ErrorTailLines,
		PaneScrollback:     b.PaneScrollback,
		GateDisclaimer:     b.GateDisclaimer,
		CancelKillGrace:    time.Duration(b.CancelKillGraceMS) * time.Millisecond,
		CancelKey:          b.Keys.Cancel,
		RerunKey:           b.Keys.Rerun,
	}

	for _, s := range b.Setup {
		out.Setup = append(out.Setup, SetupStep{Name: s.Name, Argv: resolveScriptArgv(firmwareDir, s.Cmd)})
	}
	for _, c := range b.Codegen {
		spec := CodegenSpec{Name: c.Name, Argv: resolveScriptArgv(firmwareDir, c.Cmd), Output: c.Output, Src: c.Src}
		if c.Src != "" {
			spec.SrcAbs = resolveUnder(firmwareDir, c.Src)
		}
		out.Codegen = append(out.Codegen, spec)
	}
	for _, a := range b.Artifacts {
		out.Artifacts = append(out.Artifacts, ArtifactSpec{
			Name:      a.Name,
			File:      a.File,
			PathAbs:   resolveUnder(artifactDir, a.File),
			Offset:    a.Offset,
			MinSize:   a.MinSize,
			ExactSize: a.ExactSize,
		})
	}
	return out
}

// resolveUnder joins p onto base, leaving an absolute p as-is and an empty p as base.
func resolveUnder(base, p string) string {
	if p == "" {
		return base
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// resolveScriptArgv turns a [interpreter, script, args…] command into one whose
// script element is anchored absolutely under firmwareDir (cmd[1]); the interpreter
// (cmd[0]) and any extra args pass through. A command without a script element is
// returned verbatim.
func resolveScriptArgv(firmwareDir string, cmd []string) []string {
	out := append([]string(nil), cmd...)
	if len(out) >= 2 {
		out[1] = resolveUnder(firmwareDir, out[1])
	}
	return out
}
