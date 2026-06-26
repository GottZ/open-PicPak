package build

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// codegenResult is one generator's outcome: its spec, its exit code, the output it
// produced, and any non-exit transport error (exec-not-found / context cancel).
type codegenResult struct {
	Spec  CodegenSpec
	RC    int
	Lines []Line
	Err   error // transport error (NOT a clean non-zero exit, whose code is in RC)
}

// runCodegen runs the four generators concurrently (they are independent; CMake
// guards on all four headers), maps each generator's exit code back to its header
// name, and returns the per-generator results, the coalesced output lines (a header
// line per result, in stable spec order), and the aggregate rc (the first non-zero
// generator code, or -1 on a transport error). A non-zero aggregate aborts the
// pipeline before docker — a missing header would otherwise hard-fail CMake.
func runCodegen(ctx context.Context, cfg Config, runner sshhost.Runner) ([]codegenResult, []Line, int) {
	results := make([]codegenResult, len(cfg.Codegen))
	var wg sync.WaitGroup
	for i := range cfg.Codegen {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			spec := cfg.Codegen[i]
			out, err := runner.CaptureOutput(ctx, spec.Argv)
			rc, transportErr, tail := classifyCapture(err)
			res := codegenResult{Spec: spec, RC: rc, Err: transportErr}
			for _, l := range splitLines(out) {
				res.Lines = append(res.Lines, Line{Text: l, Stream: Stdout})
			}
			if rc == 0 {
				res.Lines = append(res.Lines, Line{
					Text:   fmt.Sprintf("codegen: %s → %s ok", spec.Name, spec.Output),
					Stream: Stdout,
				})
			} else {
				res.Lines = append(res.Lines, Line{
					Text:   fmt.Sprintf("codegen: %s (%s) FAILED (rc=%d): %s", spec.Name, spec.Output, rc, tail),
					Stream: Stderr,
				})
			}
			results[i] = res
		}(i)
	}
	wg.Wait()

	var lines []Line
	rc := 0
	for _, r := range results {
		lines = append(lines, r.Lines...)
		if r.RC != 0 && rc == 0 {
			rc = r.RC
		}
	}
	return results, lines, rc
}

// classifyCapture turns a Runner.CaptureOutput error into (exit code, transport
// error, stderr tail). A clean exit is rc 0. A non-zero process exit yields its code
// and no transport error (the stderr tail is the captureError's message). Anything
// else (exec-not-found, context cancel) is a transport error with rc -1.
func classifyCapture(err error) (rc int, transportErr error, tail string) {
	if err == nil {
		return 0, nil, ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if code < 0 {
			code = -1 // killed by signal (e.g. context cancel SIGKILL)
		}
		return code, nil, strings.TrimSpace(err.Error())
	}
	return -1, err, strings.TrimSpace(err.Error())
}

// splitLines splits captured output into non-trailing-empty lines.
func splitLines(out string) []string {
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}
