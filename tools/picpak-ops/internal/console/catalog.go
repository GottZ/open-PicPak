package console

// Command is one firmware console command mirrored from the firmware source —
// firmware/main/console.c print_help (:80-99) and the handle() dispatch (:117-351).
// The catalog powers the in-TUI help overlay and input completion. It is a STATIC
// mirror checked against the firmware source, NOT typed by the device at connect
// time, so help works before/without a link. (A console.c change without a catalog
// update is a known drift risk — see the design's genhelp note.)
type Command struct {
	Name     string // dispatch token, upper-case as the firmware compares case-insensitively
	Args     string // human argument grammar
	Help     string // one-line description
	Category string // grouping for the help overlay (descriptive only)
}

// catalog is the static command table, mirrored verbatim from console.c so the
// operator sees the same surface the firmware dispatches.
var catalog = []Command{
	{"SETWIFI", "<ssid> <pass> | ADD <ssid> <pass> | LIST | DEL <slug>", "save WiFi creds / manage the multi-WiFi store", "provisioning"},
	{"SETURL", "<url>", "save the frame image URL", "provisioning"},
	{"ERASE", "", "delete the saved config", "provisioning"},
	{"INFO", "", "show status (boot count, heap, ssid/url, config state)", "status"},
	{"REFRESH", "", "fetch the image now, then sleep", "actions"},
	{"SLEEP", "[s]", "sleep without fetching (optional seconds)", "actions"},
	{"PRESS", "[n]", "run n simulated button-press cycles (test, default 1)", "actions"},
	{"LOG", "[CLEAR]", "show / clear the firmware log ring", "diagnostics"},
	{"NETCLR", "", "discard the connect cache (BSSID/IP) -> next connect is cold", "network"},
	{"OTA", "", "OTA status: running/boot/next partition + image state", "ota"},
	{"GUARD", "[CLEAR | THRESHOLD <n> | UPTIME <ms>]", "bootloop guard: status / clear / tune", "guard"},
	{"STORE", "OK | LIST | GET <k> | SET <k> <v> | DEL <k>", "littlefs key-value store", "storage"},
	{"RTC", "STAT | GET <k> | SET <k> <v> | DEL <k>", "RTC-RAM key-value (ephemeral, nulled on reset)", "storage"},
	{"NET", "SCAN | TRY <ssid> <pass> | IP | SSID | RSSI | STOP | TXPOWER [dBm]", "Wi-Fi surface / TX power", "network"},
	{"HELP", "", "show the firmware command help", "help"},
}

// Catalog returns a copy of the static command catalog (defensive: the caller cannot
// mutate the shared table).
func Catalog() []Command {
	out := make([]Command, len(catalog))
	copy(out, catalog)
	return out
}

// CommandNames returns just the command names — the input-completion source.
func CommandNames() []string {
	out := make([]string, len(catalog))
	for i, c := range catalog {
		out[i] = c.Name
	}
	return out
}
