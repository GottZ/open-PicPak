package ota

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/fleet"
	"github.com/open-picpak/picpak-ops/internal/telemetry"
)

// cmds.go wraps every DB/HTTP/file op in a tea.Cmd goroutine returning a tea.Msg — no
// blocking work in Update() (hard constraint). Each goroutine captures the per-pane ctx
// + the immutable handles at issue time; it never reads/writes pane fields.

// loadCmd reads the OTA tables (Store), the running versions (telemetry LatestFleet —
// consumed, not re-queried), and the device labels/channels (fleet cache), then
// assembles the per-device target-vs-running view via the pure BuildDeviceOTA. A
// telemetry read error is non-fatal (the OTA tables still render; running shows "?").
func (p *otaPane) loadCmd() tea.Cmd {
	ctx := p.Context()
	store := p.store
	repo := p.repo
	cache := p.cache
	prec := append([]string(nil), p.precedence...)
	cap := p.fleetCap
	return func() tea.Msg {
		state, err := store.Load(ctx)
		if err != nil {
			return otaErrMsg{err: err, at: time.Now()}
		}
		var rows []telemetry.FleetRow
		if repo != nil {
			if r, e := repo.LatestFleet(ctx, cap); e == nil {
				rows = r
			}
		}
		var cached []fleet.Device
		if cache != nil {
			cached = cache.List()
		}
		state.Devices = BuildDeviceOTA(mergeDeviceInfos(cached, rows), state, prec)
		return otaStateMsg{state: state, at: time.Now()}
	}
}

// mergeDeviceInfos unions the fleet cache (authoritative channel/label/last_seen) with
// the telemetry running rows (running_ver, report freshness). A device in the cache but
// without telemetry shows running "?" (HasReport stays the cache's last_seen); a device
// in telemetry but not the cache is added with its device-claimed channel.
func mergeDeviceInfos(cached []fleet.Device, rows []telemetry.FleetRow) []DeviceInfo {
	m := map[string]*DeviceInfo{}
	for _, d := range cached {
		m[d.Serial] = &DeviceInfo{
			Serial:    d.Serial,
			Label:     d.Label,
			Channel:   d.Channel,
			LastSeen:  d.LastSeen,
			HasReport: d.HasLastSeen(),
		}
	}
	for _, r := range rows {
		di, ok := m[r.Serial]
		if !ok {
			di = &DeviceInfo{Serial: r.Serial, Channel: r.DeviceChannel}
			m[r.Serial] = di
		}
		di.RunningVer = r.RunningVer
		di.HasReport = true
		if di.Channel == "" {
			di.Channel = r.DeviceChannel
		}
		if r.Time.After(di.LastSeen) {
			di.LastSeen = r.Time
		}
	}
	out := make([]DeviceInfo, 0, len(m))
	for _, di := range m {
		out = append(out, *di)
	}
	return out
}

// scanArtifactCmd scans the built firmware tree (version.txt + lowercase sha256 + size)
// off the loop and returns the result to prefill the register form.
func (p *otaPane) scanArtifactCmd() tea.Cmd {
	cfg := p.cfg
	return func() tea.Msg {
		art, err := ScanArtifact(cfg)
		return fwArtifactScannedMsg{art: art, err: err}
	}
}

// writeCmd runs a write op in a goroutine, marking the result with kind. The single
// in-flight guard (set by the caller) is cleared when writeResultMsg returns.
func (p *otaPane) writeCmd(kind string, fn func(ctx context.Context, w Writer) error) tea.Cmd {
	ctx := p.Context()
	w := p.writer
	return func() tea.Msg {
		if w == nil {
			return writeResultMsg{kind: kind, err: ErrWritesDisabled}
		}
		return writeResultMsg{kind: kind, err: fn(ctx, w)}
	}
}

// registerFirmwareCmd reads the app bytes off the loop (the multipart blob for the
// AdminAPIWriter; the bytes the DirectPGXWriter copies to a serving dir) and registers
// the version. The sha + size come from the prior scan (the exact bytes the firmware
// will verify); the writer re-validates the lowercase-hex format and the DB CHECK is the
// backstop.
func (p *otaPane) registerFirmwareCmd(art FirmwareArtifact, version, notes string) tea.Cmd {
	ctx := p.Context()
	w := p.writer
	blobPath := blobPathFor(p.cfg.OTA.BlobDir, p.cfg.OTA.BlobFilename, version)
	kind := "register " + version
	return func() tea.Msg {
		if w == nil {
			return writeResultMsg{kind: kind, err: ErrWritesDisabled}
		}
		blob, err := os.ReadFile(art.Path)
		if err != nil {
			return writeResultMsg{kind: kind, err: fmt.Errorf("ota: reading firmware blob: %w", err)}
		}
		req := RegisterFirmware{
			Version:   version,
			SHA256:    art.SHA256,
			SizeBytes: art.SizeBytes,
			Notes:     notes,
			Blob:      blob,
			BlobPath:  blobPath,
		}
		return writeResultMsg{kind: kind, err: w.RegisterFirmware(ctx, req)}
	}
}

// blobPathFor builds the blob_path the schema records. With a serving dir set it is
// <dir>/<version>/<filename> (absolute → the DirectPGXWriter copies the bytes there);
// without one it is the relative "<version>/<filename>" object-store-style key.
func blobPathFor(blobDir, blobName, version string) string {
	if blobName == "" {
		blobName = "firmware.bin"
	}
	if blobDir != "" {
		return filepath.Join(blobDir, version, blobName)
	}
	return filepath.Join(version, blobName)
}

func (p *otaPane) setChannelCmd(req SetChannelDefault) tea.Cmd {
	return p.writeCmd("set "+req.Channel+" default", func(ctx context.Context, w Writer) error {
		return w.SetChannelDefault(ctx, req)
	})
}

func (p *otaPane) pinCmd(req PinRollout) tea.Cmd {
	verb := "pin"
	if !req.Pinned {
		verb = "unpin"
	}
	return p.writeCmd(verb+" "+req.Serial, func(ctx context.Context, w Writer) error {
		return w.PinRollout(ctx, req)
	})
}

func (p *otaPane) setStateCmd(req SetRolloutState) tea.Cmd {
	return p.writeCmd("set state "+req.State, func(ctx context.Context, w Writer) error {
		return w.SetRolloutState(ctx, req)
	})
}
