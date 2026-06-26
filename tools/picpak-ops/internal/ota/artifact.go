package ota

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// FirmwareArtifact is the result of scanning a built firmware tree for the inputs to a
// firmware_versions registration: the version string (firmware/version.txt), the app
// binary's streaming lowercase-hex sha256, its size, and the resolved app path. The
// SHA256 is exactly what the firmware computes over the downloaded bytes, so it equals
// what the device verifies.
type FirmwareArtifact struct {
	Version   string
	SHA256    string // 64 lowercase hex (validated)
	SizeBytes int64
	Path      string
}

// ScanArtifact reads version.txt and streams the app binary's sha256 from the firmware
// tree addressed by the build-axis config: build.repo_path (made absolute for a local
// build, mirroring build.NewConfig) anchors build.artifact_dir + the build.artifacts
// entry named ota.fw_artifact_name (the app binary), and ota.version_file (repo-
// relative) carries the version. The emitted sha256 is validated lowercase-hex before
// it is returned, so a caller can register it without re-checking (the write path
// re-validates anyway; the DB CHECK is the final backstop).
func ScanArtifact(cfg *config.Config) (FirmwareArtifact, error) {
	b := cfg.Build
	o := cfg.OTA

	repo := b.RepoPath
	local := b.Host == "" || b.Host == "local"
	if local {
		if abs, err := filepath.Abs(repo); err == nil {
			repo = abs
		}
	}

	appFile, ok := artifactFile(b.Artifacts, o.FWArtifactName)
	if !ok {
		return FirmwareArtifact{}, fmt.Errorf("ota: no build.artifacts entry named %q to hash", o.FWArtifactName)
	}
	appPath := resolveUnder(resolveUnder(repo, b.ArtifactDir), appFile)
	versionPath := resolveUnder(repo, o.VersionFile)

	versionRaw, err := os.ReadFile(versionPath)
	if err != nil {
		return FirmwareArtifact{}, fmt.Errorf("ota: reading version file %s: %w", o.VersionFile, err)
	}
	version := strings.TrimSpace(string(versionRaw))
	if version == "" {
		return FirmwareArtifact{}, fmt.Errorf("ota: version file %s is empty", o.VersionFile)
	}

	fi, err := os.Stat(appPath)
	if err != nil {
		return FirmwareArtifact{}, fmt.Errorf("ota: missing firmware artifact %q: %w", o.FWArtifactName, err)
	}

	sum, err := sha256File(appPath)
	if err != nil {
		return FirmwareArtifact{}, fmt.Errorf("ota: hashing firmware artifact: %w", err)
	}
	if !ValidSHA256(sum) {
		// hex.EncodeToString is lowercase by construction; this can only fail if the
		// hash impl ever regressed — fail closed rather than register a bad digest.
		return FirmwareArtifact{}, fmt.Errorf("ota: computed sha256 %q is not 64 lowercase hex digits", sum)
	}

	return FirmwareArtifact{
		Version:   version,
		SHA256:    sum,
		SizeBytes: fi.Size(),
		Path:      appPath,
	}, nil
}

// artifactFile returns the File of the build.artifacts entry with the given name.
func artifactFile(artifacts []config.BuildArtifact, name string) (string, bool) {
	for _, a := range artifacts {
		if a.Name == name {
			return a.File, true
		}
	}
	return "", false
}

// sha256File streams the file through sha256 and returns lowercase hex.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
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

// resolveUnder joins p onto base, leaving an absolute p as-is and an empty p as base
// (mirrors build.resolveUnder so OTA and build agree on path semantics).
func resolveUnder(base, p string) string {
	if p == "" {
		return base
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}
