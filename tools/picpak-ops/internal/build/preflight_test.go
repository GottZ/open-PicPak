package build

import (
	"path/filepath"
	"testing"
)

// TestRunPreflight_Findings drives preflight against a deliberately bare firmware
// tree and asserts it materializes the right blocking findings from config-derived
// targets: a components-missing block carrying the "setup" fix, a missing fonts dir,
// and a missing codegen Berry source.
func TestRunPreflight_Findings(t *testing.T) {
	tmp := t.TempDir()
	cfg := Config{
		Host:               "local",
		Local:              true,
		FirmwareDir:        tmp, // nothing populated under it
		RequiredComponents: []string{"berry", "littlefs", "qrcodegen"},
		FontsDir:           filepath.Join(tmp, "screens-src", "fonts"),
		Codegen: []CodegenSpec{
			{Name: "render", Src: "main/render.be", SrcAbs: filepath.Join(tmp, "main", "render.be")},
		},
		PythonBin:         "python3",
		PythonImportProbe: "import PIL",
		DockerCmd:         "docker",
		DockerImage:       "espressif/idf:v5.5.3",
	}

	findings := runPreflight(ctxBG(), cfg, localRunnerForTest(t))
	if !hasBlocking(findings) {
		t.Fatalf("expected blocking preflight findings, got %+v", findings)
	}

	var sawSetup, sawFonts, sawSrc bool
	for _, f := range findings {
		if f.Fix == "setup" && f.Severity == SeverityBlock {
			sawSetup = true
		}
		if containsFold(f.Text, "fonts dir") {
			sawFonts = true
		}
		if containsFold(f.Text, "codegen source") {
			sawSrc = true
		}
	}
	if !sawSetup {
		t.Errorf("expected a components-missing block with Fix=setup; findings=%+v", findings)
	}
	if !sawFonts {
		t.Errorf("expected a missing-fonts block; findings=%+v", findings)
	}
	if !sawSrc {
		t.Errorf("expected a missing-codegen-source block; findings=%+v", findings)
	}
}
