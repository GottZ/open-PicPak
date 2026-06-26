package build

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// runPreflight materializes each hard precondition into an early, one-line finding
// (§2.4) so the operator gets "run setup-berry.sh" instead of a three-screen CMake
// trace. Every probe's TARGET is derived from a config key (required_components,
// codegen[].src, fonts_dir, python_import_probe, docker_image), so a config path
// override propagates into the check; only the check KINDS are code. The probes run
// through the single Runner (reachability/pillow/docker) or the shared fileProbe
// (component/source/fonts existence), so local and remote share the logic.
//
// Severity: a missing docker image is warn-only (docker auto-pulls); everything else
// blocks. A missing component set additionally carries Fix "setup" so the pane can
// offer to run the one-time setup scripts on confirm.
func runPreflight(ctx context.Context, cfg Config, runner sshhost.Runner) []Finding {
	var findings []Finding
	probe := newFileProbe(ctx, cfg, runner)

	// Remote reachability gate first: if the host is unreachable nothing else can be
	// checked, so this short-circuits.
	if !cfg.Local {
		if _, err := runner.CaptureOutput(ctx, []string{"true"}); err != nil {
			return append(findings, blockFinding(StagePreflight, "",
				"build host %q is unreachable: %v", cfg.Host, err))
		}
	}

	// docker image present? warn-only — the build pulls it otherwise (slow first run).
	if _, err := runner.CaptureOutput(ctx, []string{cfg.DockerCmd, "image", "inspect", cfg.DockerImage}); err != nil {
		findings = append(findings, warnFinding(StagePreflight,
			"docker image %q not present on the build host; the build will pull it (slow on first run)", cfg.DockerImage))
	}

	// Pillow present? codegen (gen_screens/gen_font16) needs PIL.
	if _, err := runner.CaptureOutput(ctx, []string{cfg.PythonBin, "-c", cfg.PythonImportProbe}); err != nil {
		findings = append(findings, blockFinding(StagePreflight, "",
			"Pillow missing on the build host (%s -c %q failed); install it (codegen needs PIL)", cfg.PythonBin, cfg.PythonImportProbe))
	}

	// Required components populated? block + offer setup.
	var missing []string
	for _, c := range cfg.RequiredComponents {
		dir := resolveUnder(cfg.FirmwareDir, filepath.Join("components", c))
		if _, err := probe.size(dir); err != nil {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		findings = append(findings, blockFinding(StagePreflight, "setup",
			"missing firmware components: %s — run the one-time setup scripts (network) on confirm", strings.Join(missing, ", ")))
	}

	// Berry sources the render/policy generators read.
	for _, spec := range cfg.Codegen {
		if spec.SrcAbs == "" {
			continue
		}
		if _, err := probe.size(spec.SrcAbs); err != nil {
			findings = append(findings, blockFinding(StagePreflight, "",
				"codegen source %s for generator %q is missing", spec.Src, spec.Name))
		}
	}

	// Bundled fonts for the bitmap generators.
	if _, err := probe.size(cfg.FontsDir); err != nil {
		findings = append(findings, blockFinding(StagePreflight, "",
			"fonts dir %s is missing (gen_screens/gen_font16 need it)", cfg.FontsDir))
	}

	return findings
}

// runSetup runs the one-time component-setup scripts (offered on confirm after a
// components-missing finding; slow + network-touching). It runs each script to
// completion in configured order and returns the coalesced output plus the first
// non-zero exit code.
func runSetup(ctx context.Context, cfg Config, runner sshhost.Runner) ([]Line, int) {
	var lines []Line
	rc := 0
	for _, s := range cfg.Setup {
		lines = append(lines, Line{Text: "setup: " + s.Name + " …", Stream: Stdout})
		out, err := runner.CaptureOutput(ctx, s.Argv)
		code, _, tail := classifyCapture(err)
		for _, l := range splitLines(out) {
			lines = append(lines, Line{Text: l, Stream: Stdout})
		}
		if code != 0 {
			lines = append(lines, Line{Text: fmt.Sprintf("setup: %s FAILED (rc=%d): %s", s.Name, code, tail), Stream: Stderr})
			if rc == 0 {
				rc = code
			}
			break // a failed precondition setup aborts the rest
		}
		lines = append(lines, Line{Text: "setup: " + s.Name + " ok", Stream: Stdout})
	}
	return lines, rc
}

func blockFinding(stage Stage, fix, format string, a ...any) Finding {
	return Finding{Stage: stage, Severity: SeverityBlock, Fix: fix, Text: fmt.Sprintf(format, a...)}
}

func warnFinding(stage Stage, format string, a ...any) Finding {
	return Finding{Stage: stage, Severity: SeverityWarn, Text: fmt.Sprintf(format, a...)}
}
