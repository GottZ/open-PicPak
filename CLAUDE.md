# open-picpak — agent & contributor conventions

Open firmware, tooling and documentation for the PicPak 4.2″ e-ink frame (ESP32-C3, 400×300 BWRY,
four colours K/W/Y/R). Clean-room, vendor-independent, fully public. This file is the binding
convention set for **any agent or contributor** working in this repo. Deeper planning, inventory and
design live in the gitignored `.project/` directory (not part of this repo).

## Air-gap — the first rule
This is a **public, pushed** repository. **Never commit real identifiers** in code, comments, commit
messages or docs: serial numbers, MAC addresses, IP addresses, Wi-Fi SSIDs, secret tokens or paths,
private host/domain names, or reverse-engineering provenance. Use **format-preserving placeholders**
(stub table in `documentation/device.md`). A grep gate (`tools/check-airgap.sh`) blocks known private
patterns on staged/changed files — run it before committing.

## Language
**English throughout** — code, comments, commit messages, docs, this file.

## Engineering method
- **One change per wave, validated live.** Gate = build green **plus a wave-specific probe**;
  **on-device** when hardware is touched (measure, do not assume).
- **Pausability:** every wave ends committed / tested / deployable; no intermediate state forces the
  next wave. Flags default-off, bridges kept until an explicit cut-over wave.
- **Mechanism = code / policy = data:** no magic values. Intervals, thresholds, URLs, rotation live
  in NVS or server config, not as code constants.

## Verification discipline
- **Verify against the primary source** (the real code / the live device) — not docs, comments, or
  agent/LLM synthesis. "Build green" is a regression gate, **not** a correctness or display proof;
  display/hardware effects count as proven only when seen on the device.
- **Protective/safety properties as negatively-probed tests:** red first (the property breaks), then
  green.

## Flash / NVS discipline — never full-erase
The **NVS partition at `0x9000`** carries the **factory per-device data**: RF calibration, base MAC,
serial number, BT config. This firmware **coexists with that NVS additively** — a normal flash + boot
leaves it intact. A **full erase** (`erase-flash` / `erase-region` over `0x9000`) destroys it
**irrecoverably** (only a prior full-flash backup can restore it) and breaks a clean stock re-flash,
since stock and this firmware share the same NVS region. **Always flash write-only**
(`0x0` / `0x8000` / `0x10000` / `0x20000`); never erase.
