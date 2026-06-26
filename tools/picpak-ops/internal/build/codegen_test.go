package build

import (
	"strings"
	"testing"
)

// TestRunCodegen_RCMapping proves each generator's exit code maps back to its result
// (and header label), and the aggregate rc is the first non-zero code. It runs real
// shell commands through the local Runner so the exit codes are genuine.
func TestRunCodegen_RCMapping(t *testing.T) {
	runner := localRunnerForTest(t)
	cfg := Config{
		Codegen: []CodegenSpec{
			{Name: "screens", Output: "main/screens.h", Argv: []string{"sh", "-c", "echo wrote main/screens.h"}},
			{Name: "policy", Output: "main/policy_script.h", Argv: []string{"sh", "-c", "exit 3"}},
		},
	}

	results, lines, rc := runCodegen(ctxBG(), cfg, runner)
	if rc != 3 {
		t.Fatalf("aggregate rc = %d, want 3 (the failing generator's code)", rc)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 codegen results, got %d", len(results))
	}
	byName := map[string]codegenResult{}
	for _, r := range results {
		byName[r.Spec.Name] = r
	}
	if byName["screens"].RC != 0 {
		t.Errorf("screens rc = %d, want 0", byName["screens"].RC)
	}
	if byName["policy"].RC != 3 {
		t.Errorf("policy rc = %d, want 3", byName["policy"].RC)
	}

	joined := renderLines(lines)
	if !strings.Contains(joined, "policy (main/policy_script.h) FAILED (rc=3)") {
		t.Fatalf("missing rc-mapped failure label for policy: %q", joined)
	}
	if !strings.Contains(joined, "screens → main/screens.h ok") {
		t.Fatalf("missing success label for screens: %q", joined)
	}
}

// TestRunCodegen_AllGreen confirms a fully-successful codegen has rc 0.
func TestRunCodegen_AllGreen(t *testing.T) {
	runner := localRunnerForTest(t)
	cfg := Config{
		Codegen: []CodegenSpec{
			{Name: "a", Output: "main/a.h", Argv: []string{"sh", "-c", "echo a"}},
			{Name: "b", Output: "main/b.h", Argv: []string{"sh", "-c", "echo b"}},
		},
	}
	_, _, rc := runCodegen(ctxBG(), cfg, runner)
	if rc != 0 {
		t.Fatalf("all-green codegen rc = %d, want 0", rc)
	}
}

func renderLines(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text)
		b.WriteByte('\n')
	}
	return b.String()
}
