package buildpane

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/open-picpak/picpak-ops/internal/build"
	"github.com/open-picpak/picpak-ops/internal/config"
)

// viewColors are the chrome roles the build view styles with. They come from the
// config theme map (no color literal in this package); a missing role resolves to
// the zero color (no styling), so the view never bakes an ANSI index of its own.
type viewColors struct {
	working color.Color
	ok      color.Color
	err     color.Color
	muted   color.Color
	border  color.Color
}

// resolveColors maps theme roles to the build view's palette.
func resolveColors(cfg *config.Config) viewColors {
	c := func(role string) color.Color { return lipgloss.Color(cfg.Theme[role]) }
	return viewColors{
		working: c("status_working"),
		ok:      c("status_active"),
		err:     c("status_error"),
		muted:   c("status_quiet"),
		border:  c("border"),
	}
}

// displayStages is the fixed stage-bar order (setup folds into preflight).
var displayStages = []build.Stage{
	build.StagePreflight,
	build.StageCodegen,
	build.StageDockerBuild,
	build.StageVerify,
}

// View renders the pane body: the gate-disclaimer banner, the stage bar, an
// error-tail box on a failed terminal, and the live output tail — sized to the last
// SetSize. The layout draws the surrounding border/title.
func (p *buildPane) View() string {
	w, h := p.Size()
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}

	var blocks []string

	if p.runnerErr != nil {
		blocks = append(blocks, lipgloss.NewStyle().Foreground(p.colors.err).Render(
			"build host error: "+p.runnerErr.Error()))
	}

	blocks = append(blocks, p.renderBanner(w))
	blocks = append(blocks, p.renderStageBar())

	// Failure box (above the scrollback) on a non-green terminal.
	if p.result != nil && !p.result.Green() && !p.result.Canceled {
		blocks = append(blocks, p.renderErrorBox(w))
	}
	if p.result != nil && p.result.Canceled {
		blocks = append(blocks, lipgloss.NewStyle().Foreground(p.colors.muted).Render("build canceled."))
	}
	if p.offerSetup && !p.live {
		blocks = append(blocks, lipgloss.NewStyle().Foreground(p.colors.working).Render(
			fmt.Sprintf("components missing — press %q to run the setup scripts and rebuild", p.cfg.RerunKey)))
	}

	header := strings.Join(blocks, "\n")
	headerLines := strings.Count(header, "\n") + 1

	// Remaining height goes to the live output tail.
	tailHeight := h - headerLines - 1
	if tailHeight < 1 {
		tailHeight = 1
	}
	tail := p.renderTail(tailHeight)

	return header + "\n" + tail
}

// renderBanner draws the mandatory gate disclaimer: the wording is config (Policy =
// Data), the layout is fixed and never removable. It is emphasized on a green
// terminal, muted while a build is in flight.
func (p *buildPane) renderBanner(w int) string {
	text := p.cfg.GateDisclaimer
	style := lipgloss.NewStyle().Foreground(p.colors.muted)
	if p.result != nil && p.result.Green() {
		style = lipgloss.NewStyle().Foreground(p.colors.ok).Bold(true)
	}
	return style.Render(truncate(text, w))
}

// renderStageBar renders preflight → codegen → docker-build → verify with each
// stage's live/done/pending state and its recorded duration.
func (p *buildPane) renderStageBar() string {
	timing := map[build.Stage]build.StageTiming{}
	for _, t := range p.stages {
		timing[t.Stage] = t
	}
	parts := make([]string, 0, len(displayStages))
	for _, st := range displayStages {
		label := st.String()
		switch {
		case p.live && p.state == st:
			parts = append(parts, lipgloss.NewStyle().Foreground(p.colors.working).Bold(true).Render("▶ "+label))
		default:
			if t, ok := timing[st]; ok {
				glyph, col := "✓", p.colors.ok
				if t.RC != 0 {
					glyph, col = "✗", p.colors.err
				}
				dur := t.Duration.Round(100_000_000) // 0.1s
				parts = append(parts, lipgloss.NewStyle().Foreground(col).Render(
					fmt.Sprintf("%s %s (%s)", glyph, label, dur)))
			} else {
				parts = append(parts, lipgloss.NewStyle().Foreground(p.colors.muted).Render("· "+label))
			}
		}
	}
	return strings.Join(parts, "   ")
}

// renderErrorBox draws the bordered, color-emphasized failure tail above the
// scrollback, naming the failing stage + rc so the operator sees the failure class
// without scrolling.
func (p *buildPane) renderErrorBox(w int) string {
	r := p.result
	stageName := r.Stage.String()
	for _, t := range r.Stages {
		if t.RC != 0 {
			stageName = t.Stage.String()
			break
		}
	}
	title := fmt.Sprintf("%s failed (rc=%d)", stageName, r.RC)

	tail := p.errTail
	if len(tail) == 0 {
		tail = r.ErrTail
	}
	body := strings.Join(tail, "\n")
	if strings.TrimSpace(body) == "" {
		body = "(no error output captured)"
	}

	boxW := w - 2
	if boxW < 10 {
		boxW = 10
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(p.colors.err).
		Foreground(p.colors.err).
		Padding(0, 1).
		Width(boxW)
	return box.Render(lipgloss.NewStyle().Bold(true).Render(title) + "\n" + body)
}

// renderTail renders the last n ring lines (the live output), each truncated to the
// pane width.
func (p *buildPane) renderTail(n int) string {
	lines := p.ring.Lines()
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	w, _ := p.Size()
	if w <= 0 {
		w = 80
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = truncate(l, w)
	}
	return strings.Join(out, "\n")
}

// truncate cuts s to a max display width (ANSI-naive; the build output is plain).
func truncate(s string, w int) string {
	if w <= 1 || lipgloss.Width(s) <= w {
		return s
	}
	// Trim byte-wise to roughly w-1 then add an ellipsis; adequate for ASCII logs.
	if len(s) > w-1 {
		return s[:w-1] + "…"
	}
	return s
}
