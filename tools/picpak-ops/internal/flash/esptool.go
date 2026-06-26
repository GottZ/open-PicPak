package flash

import (
	"fmt"
	"strings"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// writeFlashVerb is the esptool subcommand flash uses. It is FIXED in code — never
// config — so no operator key can swap it for an erase verb. esptool 9.x accepts the
// dashed spelling.
const writeFlashVerb = "write-flash"

// dashReset translates an esptool reset-mode token from the IDF manifest's underscored
// spelling (hard_reset / default_reset) to the esptool-9.x dashed form
// (hard-reset / default-reset). config.flash.before/after already carry the dashed
// values, so this is a defensive normalization of whatever the field holds.
func dashReset(mode string) string {
	return strings.ReplaceAll(strings.TrimSpace(mode), "_", "-")
}

// BuildEsptoolArgv assembles the full esptool argv for one device. hostDir is the
// directory the on-host artifact paths are anchored under (the staging dir for scp,
// the build/artifact dir for local/build_on_host); each entry's relative File is
// joined onto it so esptool resolves the bins without a CWD wrapper.
//
//	<esptool_cmd…> --chip <chip> -p <port> --before <before> --after <after>
//	  write-flash --flash-mode <mode> --flash-size <size> --flash-freq <freq>
//	  [extra_args…] <offset> <path> …            (offset-sorted; app last)
//
// Offsets are emitted as 0x-hex. The NVS offset is never present (it is not a
// build.artifacts entry and the sorted set carries only those). Reset modes are
// dashed (underscore→dash translation here).
func BuildEsptoolArgv(fc config.Flash, port, hostDir string, ws *WriteSet) []string {
	argv := make([]string, 0, len(fc.EsptoolCmd)+12+2*len(ws.Entries))
	argv = append(argv, fc.EsptoolCmd...)
	argv = append(argv, "--chip", fc.Chip)
	argv = append(argv, "-p", port)
	argv = append(argv, "--before", dashReset(fc.Before))
	argv = append(argv, "--after", dashReset(fc.After))
	argv = append(argv, writeFlashVerb)
	argv = append(argv, "--flash-mode", fc.FlashMode)
	argv = append(argv, "--flash-size", fc.FlashSize)
	argv = append(argv, "--flash-freq", fc.FlashFreq)
	argv = append(argv, fc.ExtraArgs...)
	for _, e := range ws.Entries {
		argv = append(argv, fmt.Sprintf("%#x", e.Offset), resolveUnder(hostDir, e.File))
	}
	return argv
}
