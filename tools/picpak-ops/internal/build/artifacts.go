package build

// Artifact is one verified flash artifact: the on-host path, its flash offset, and
// the observed size. It is the value object the Flash axis (offsets + paths) and the
// OTA axis (the app's sha256) consume after a green build.
type Artifact struct {
	Name   string
	File   string // build-dir-relative path (matches flasher_args.json flash_files)
	Path   string // absolute on-host path
	Offset string // hex, as configured
	Size   int64
}

// ArtifactSet is the verified output of a green build: the ordered artifacts (in
// configured offset order), the app binary and its lowercase-hex SHA-256, and the
// firmware version. AppSHA256 is the value OTA/firmware-register hand to the device
// as X-Firmware-SHA256 — it MUST stay lowercase (ota.c compares via strcmp, the
// backend CHECK is ^[0-9a-f]{64}$); crypto/sha256 + hex.EncodeToString already emits
// lowercase, and nothing here uppercases it.
type ArtifactSet struct {
	Artifacts []Artifact
	App       Artifact
	AppSHA256 string // 64-char lowercase hex
	Version   string // firmware/version.txt, if read; "" otherwise
}

// AppArtifact returns the app entry by name "app", falling back to the last artifact
// (configured offset order puts the app last). Returns the zero Artifact if empty.
func (s ArtifactSet) AppArtifact() Artifact {
	for _, a := range s.Artifacts {
		if a.Name == "app" {
			return a
		}
	}
	if n := len(s.Artifacts); n > 0 {
		return s.Artifacts[n-1]
	}
	return Artifact{}
}
