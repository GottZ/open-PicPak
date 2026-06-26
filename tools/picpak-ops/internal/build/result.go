package build

import "time"

// Stage is one pipeline stage, in execution order. The values are also the stage-bar
// labels the pane renders.
type Stage int

const (
	StageIdle Stage = iota
	StagePreflight
	StageCodegen
	StageDockerBuild
	StageVerify
	StageDone
	StageFailed
	StageCanceled
)

// String renders the stage for the pane's stage bar and failure labels.
func (s Stage) String() string {
	switch s {
	case StageIdle:
		return "idle"
	case StagePreflight:
		return "preflight"
	case StageCodegen:
		return "codegen"
	case StageDockerBuild:
		return "docker-build"
	case StageVerify:
		return "verify"
	case StageDone:
		return "done"
	case StageFailed:
		return "failed"
	case StageCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// Terminal reports whether a stage value is an end state (no further stage follows).
func (s Stage) Terminal() bool { return s == StageDone || s == StageFailed || s == StageCanceled }

// Severity classifies a Finding. A Block aborts the pipeline (a hard precondition or
// a verify mismatch); a Warn is surfaced but does not stop the build (e.g. a missing
// docker image, which docker auto-pulls).
type Severity int

const (
	SeverityWarn Severity = iota
	SeverityBlock
)

// String renders the severity for the pane.
func (s Severity) String() string {
	if s == SeverityBlock {
		return "block"
	}
	return "warn"
}

// Finding is one actionable preflight/verify result line: a one-line statement of a
// failed precondition or a verify mismatch, with the stage that produced it. Fix
// names an optional remediation the pane can offer (currently only "setup": run the
// component-setup scripts on confirm).
type Finding struct {
	Stage    Stage
	Severity Severity
	Text     string
	Fix      string // "" or "setup"
}

// Stream distinguishes stdout from stderr in a coalesced output batch so the pane
// can weight stderr into the failure tail. It mirrors sshhost.Stream for the lines
// the pipeline emits itself (codegen/preflight/verify); docker output arrives as
// sshhost.RunOutputMsg directly.
type Stream int

const (
	Stdout Stream = iota
	Stderr
)

// Line is one coalesced output line with its stream.
type Line struct {
	Text   string
	Stream Stream
}

// StageTiming records one stage's outcome for the result summary.
type StageTiming struct {
	Stage    Stage
	RC       int
	Duration time.Duration
}

// --- messages (delivered to the pane's Update as the Payload of an addressed
// app.PaneMsg the pipeline goroutine pushes via the Sender, K2). ---

// StageEnteredMsg announces the pipeline advanced into a stage.
type StageEnteredMsg struct {
	Stage Stage
	At    time.Time
}

// OutputLinesMsg carries a coalesced batch of output lines (wm R1) the pipeline
// produced for a stage (codegen/preflight/verify). Docker output does not flow
// through here — it streams as sshhost.RunOutputMsg straight to the pane.
type OutputLinesMsg struct {
	Stage Stage
	Lines []Line
}

// StageDoneMsg reports a stage finished with an exit code.
type StageDoneMsg struct {
	Stage    Stage
	RC       int
	Duration time.Duration
}

// FindingMsg carries one preflight/verify finding.
type FindingMsg struct {
	Finding Finding
}

// BuildDoneMsg is the single terminal message. Result is the immutable outcome the
// pane stores and renders.
type BuildDoneMsg struct {
	Result BuildResult
}

// BuildResult is the immutable terminal outcome of one pipeline run.
type BuildResult struct {
	RC        int           // 0 = green; non-zero = the failing stage's code (or -1 on cancel)
	Stage     Stage         // terminal stage (Done/Failed/Canceled)
	Stages    []StageTiming // per-stage timing + rc
	Artifacts *ArtifactSet  // non-nil only on a green build (verify passed)
	Findings  []Finding     // blocking + warning findings accumulated across stages
	ErrTail   []string      // last stderr-weighted lines on failure (for the pane's tail box)
	Err       error         // a transport/exec error (distinct from a clean non-zero rc)
	Canceled  bool
}

// Green reports whether the build produced a verified artifact set.
func (r BuildResult) Green() bool { return r.RC == 0 && r.Stage == StageDone && r.Artifacts != nil }
