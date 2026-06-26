package config

import "time"

// defaults returns the single compiled defaults table. This is the ONLY place a
// literal default may live; every consuming axis reads its values from the
// resolved *Config, never from an in-package constant of its own.
//
// The file → env → CLI merge in load.go applies on top of this table, so a key
// absent from the operator's file keeps the default here. Defaults are
// non-secret and air-gap-safe: the public Espressif VID:PID (303a:1001) and
// image/path defaults are working values, while every secret/host-bearing field
// (database.dsn, ota.admin_api_*, hosts, devices) defaults to empty so an
// operator must supply it.
func defaults() *Config {
	dur := func(d time.Duration) Duration { return Duration(d) }

	return &Config{
		Schema: SchemaVersion,

		Reload: Reload{
			Watch:    true,
			Debounce: dur(500 * time.Millisecond),
		},

		UI: UI{
			Mouse:            "cell",
			ReportFocus:      true,
			Altscreen:        true,
			TeardownDeadline: dur(5 * time.Second),
			ScrollbackLines:  5000,
			TabBar:           "top",
			StatusBar:        true,
			StartupPanes:     nil,
			Theme:            "default",
		},

		Keys: Keys{
			Quit:      []string{"ctrl+c", "q"},
			Help:      []string{"?"},
			NextTab:   []string{"tab", "ctrl+n"},
			PrevTab:   []string{"shift+tab", "ctrl+p"},
			FocusN:    "alt+",
			Spawn:     map[string][]string{},
			ClosePane: []string{"ctrl+w"},
			Split:     []string{"ctrl+s"},
		},

		// wm-shell §5 theme role names with ANSI-index defaults (terminal-honest,
		// theme-portable). UI colors, BWRY-agnostic.
		Theme: map[string]string{
			"border":         "240",
			"focus_border":   "205",
			"tab_active":     "205",
			"tab_inactive":   "240",
			"status_active":  "42",
			"status_working": "214",
			"status_quiet":   "244",
			"status_error":   "196",
			"dirty":          "214",
		},

		Database: Database{
			DSN:              "", // required; secret + host-bearing
			MaxConns:         4,
			ConnectTimeout:   dur(5 * time.Second),
			StatementTimeout: dur(15 * time.Second),
			ReadOnly:         true,
		},

		SSH: SSH{
			Binary: "ssh",
			BaseArgs: []string{
				"-o", "BatchMode=yes",
				"-o", "ConnectTimeout=8",
				"-o", "ControlMaster=auto",
				"-o", "ControlPersist=120",
			},
			ControlPath:    "~/.ssh/cm-picpak-%r@%h:%p",
			ConnectTimeout: dur(8 * time.Second),
			CommandTimeout: 0, // no ceiling — flash/console run long
		},

		Poll: Poll{
			Interval:        dur(30 * time.Second),
			Parallel:        4,
			Command:         []string{"ls", "-l", "/dev/serial/by-id/"},
			MatchRegex:      "usb-Espressif_USB_JTAG.*-if00",
			MACRegex:        "([0-9A-Fa-f]{2}([:_-]?[0-9A-Fa-f]{2}){5})",
			TTYGlob:         "/dev/ttyACM*",
			VendorID:        "303a", // public Espressif ESP32-C3 USB-Serial-JTAG VID
			ProductID:       "1001", // public Espressif ESP32-C3 USB-Serial-JTAG PID
			ConfirmCommand:  []string{"udevadm", "info", "-q", "property", "-n", "%TTY%"},
			ConfirmRequired: true,
			ConfirmCacheTTL: dur(5 * time.Minute),
		},

		Run: Run{
			ScrollbackLines: 5000,
			LineMaxBytes:    8192,
		},

		Reconnect: Reconnect{
			BackoffInitial: dur(1 * time.Second),
			BackoffMax:     dur(60 * time.Second),
			BackoffFactor:  2.0,
			FailThreshold:  3,
		},

		Hosts: nil, // operator-supplied; example carries placeholders only

		Build: Build{
			Host:        "local",
			RepoPath:    ".",
			FirmwareDir: "firmware",
			DockerImage: "espressif/idf:v5.5.3",
			DockerCmd:   "docker",
			Target:      "esp32c3",
			BuildCmd:    []string{"bash", "-lc", "idf.py set-target {target} build"},
			DockerRunArgs: []string{
				"run", "--rm",
				"--name", "{name}",
				"-v", "{repo}/firmware:/project",
				"-w", "/project",
			},
			ContainerNameTmpl: "picpak-ops-build-{runid}",
			PythonBin:         "python3",
			PythonImportProbe: "import PIL",
			StatBin:           "stat",
			Sha256Bin:         "sha256sum",
			FontsDir:          "screens-src/fonts",
			Setup: []BuildStep{
				{Name: "berry", Cmd: []string{"bash", "scripts/setup-berry.sh"}, Cwd: "firmware"},
				{Name: "littlefs", Cmd: []string{"bash", "scripts/setup-littlefs.sh"}, Cwd: "firmware"},
				{Name: "qrcodegen", Cmd: []string{"bash", "scripts/setup-qrcodegen.sh"}, Cwd: "firmware"},
			},
			// The FOUR host generators CMake guards on (main/CMakeLists.txt :6/:10/:14/:18).
			// Cmd is [interpreter, script] with the script path relative to firmware_dir;
			// the generators resolve their own output via __file__, so cwd is informational.
			Codegen: []BuildStep{
				{Name: "screens", Cmd: []string{"python3", "screens-src/gen_screens.py"}, Cwd: "firmware", Output: "main/screens.h"},
				{Name: "render", Cmd: []string{"python3", "host/gen_render.py"}, Cwd: "firmware", Output: "main/render_script.h", Src: "main/render.be"},
				{Name: "font16", Cmd: []string{"python3", "host/gen_font16.py"}, Cwd: "firmware", Output: "main/font16.h"},
				{Name: "policy", Cmd: []string{"python3", "host/gen_policy.py"}, Cwd: "firmware", Output: "main/policy_script.h", Src: "main/policy.be"},
			},
			RequiredComponents: []string{"berry", "littlefs", "qrcodegen"},
			ArtifactDir:        "firmware/build",
			Artifacts: []BuildArtifact{
				{Name: "bootloader", File: "bootloader/bootloader.bin", Offset: "0x0", MinSize: 16384},
				{Name: "partitions", File: "partition_table/partition-table.bin", Offset: "0x8000", ExactSize: 3072},
				{Name: "otadata", File: "ota_data_initial.bin", Offset: "0x10000", ExactSize: 8192},
				{Name: "app", File: "picpak_fw.bin", Offset: "0x20000", MinSize: 1048576},
			},
			FlasherArgsPath:   "build/flasher_args.json",
			AppArtifact:       "build/picpak_fw.bin",
			OTASlotSize:       4194304, // 0x400000 ota_0 (partitions.csv)
			ErrorTailLines:    40,
			PaneScrollback:    5000,
			GateDisclaimer:    "BUILD GREEN — regression gate only. Not a correctness/display proof; verify on a device.",
			CancelKillGraceMS: 2000,
			Keys: BuildKeys{
				Start:  "b",
				Cancel: "x",
				Rerun:  "r",
			},
		},

		Flash: Flash{
			EsptoolCmd:         []string{"esptool"},
			Chip:               "esp32c3",
			Before:             "default-reset",
			After:              "hard-reset",
			FlashMode:          "dio",
			FlashSize:          "16MB",
			FlashFreq:          "80m",
			ExtraArgs:          []string{},
			RunOverSSH:         true,
			NVSPreserveOffset:  "0x9000",
			ForbidErase:        true, // not disableable; validation rejects false
			StageMode:          "scp",
			StageRemoteDir:     "",
			StageSkipUnchanged: true,
			Concurrency:        1,
			RunTimeout:         dur(5 * time.Minute),
			RetryAuto:          false,
			RetryMax:           1,
			TailLines:          40,
			Keys: FlashKeys{
				Run:     "f",
				Retry:   "r",
				Abort:   "x",
				Console: "c",
			},
		},

		Console: Console{
			SSHCommand:           "ssh",
			SSHArgs:              []string{"-T", "{host}"},
			RemoteCommand:        "stty -F {port} {baud} raw -echo -ixon; trap 'kill $catpid 2>/dev/null' EXIT HUP INT TERM; cat {port} & catpid=$!; cat > {port}; kill $catpid 2>/dev/null",
			Baud:                 115200,
			AutoConnect:          false,
			Reconnect:            true,
			ReconnectBackoffMS:   1500,
			ReconnectMaxAttempts: 5,
			ScrollbackLines:      5000,
			LineEndings:          "crlf",
			LocalEcho:            false,
			QuietTimeoutMS:       2000,
			BannerMatch:          "=== PicPak Setup Console ===",
			ConfirmConnect:       true,
			SessionLogDir:        "",
			Keys: ConsoleKeys{
				Submit:      "enter",
				HistoryPrev: "up",
				HistoryNext: "down",
				ScrollUp:    "pgup",
				ScrollDown:  "pgdown",
				Search:      "/",
				Clear:       "ctrl+l",
				Reconnect:   "ctrl+r",
				Disconnect:  "ctrl+d",
				Help:        "f1",
			},
		},

		OTA: OTA{
			AdminAPIURL:      "", // host-bearing; empty ⇒ write features disabled
			AdminAPIToken:    "", // secret
			AdminAPITimeout:  dur(10 * time.Second),
			AllowDirectWrite: false,
			BlobDir:          "",
			BlobFilename:     "firmware.bin",
			ServeURLTemplate: "",
			BuildDir:         "firmware/build",
			VersionFile:      "firmware/version.txt",
			FWArtifactName:   "app",
			DefaultChannel:   "stable",
			MaxOTATries:      0,
			ResolvePrecedence: []string{
				"pinned-serial", "serial", "channel", "default",
			},
			BehindIsWarning: true,
			RefreshInterval: dur(30 * time.Second),
			Keys: map[string][]string{
				"register":     {"r"},
				"pin":          {"p"},
				"unpin":        {"u"},
				"set_channel":  {"C"},
				"toggle_state": {"space"},
				"mark_done":    {"D"},
				"refresh":      {"R"},
				"switch_table": {"tab"},
			},
		},

		Telemetry: Telemetry{
			// window/refresh_interval are the reconciled names for the 08-telemetry
			// history_window / poll_interval_focused concepts (one key per concept, no
			// duplicate); the axis-canonical defaults are 72h / 30s.
			Window:                 dur(72 * time.Hour),
			RefreshInterval:        dur(30 * time.Second),
			LogTailLines:           500,
			LogFollowInterval:      dur(30 * time.Second),
			PollIntervalBackground: dur(5 * time.Minute),
			StaleAfter:             dur(3 * time.Hour),
			HistoryMaxRows:         500,
			FleetMaxDevices:        1000,
			LowBattPct:             15,
			BadBootsWarn:           2,
			BrownoutResetReasons:   []string{"brownout"},
			BrownoutOTARRCodes:     []int{9},
			LowBattIncludesUSB:     false,
			ChannelMismatchWarn:    true,
			AgeFormat:              "relative",
			BattUnit:               "V",
			SparklineMetrics:       []string{"batt_pct", "uptime_ms", "bad_boots"},
			// telemetry.refresh is "R" (NOT "r" — hosts.poll owns "r"); per-pane scope.
			Keys: map[string][]string{
				"refresh":     {"R"},
				"select_next": {"j"},
				"select_prev": {"k"},
				"copy_serial": {"y"},
			},
		},

		Logs: Logs{
			LineSeparator:        "|",
			PollInterval:         dur(3 * time.Second),
			PollBatch:            500,
			BackfillPage:         200,
			LiveRingLines:        5000,
			HistoryMaxRows:       50000,
			DefaultFilterSerials: []string{},
			DefaultSources:       []string{"telemetry", "ota-snapshot"},
			ShowGapMarkers:       true,
			GapMarkerFormat:      "── boot {from} → {to} (gap) ──",
			ShowSuspectMarkers:   true,
			SuspectMarker:        "⚠ suspect",
			FollowDefault:        true,
			ErrorBackoff:         dur(10 * time.Second),
			ErrorBackoffMax:      dur(2 * time.Minute),
			TimestampFormat:      "15:04:05",
			Timezone:             "Local",
			ColorizeBy:           "serial",
			// Palette default lives here as the single compiled defaults location,
			// so "no magic colors in code" holds in the logs package.
			Palette:      []string{"39", "208", "120", "213", "227", "117", "210", "156"},
			QueryTimeout: dur(5 * time.Second),
			// logs.keys is a distinct (per-pane) K7 scope, so reusing f/d/s/y with other
			// panes is legal; within this scope all seven actions are distinct.
			Keys: map[string][]string{
				"follow":        {"f"},
				"device_filter": {"d"},
				"source_cycle":  {"s"},
				"clear_view":    {"x"},
				"copy":          {"y"},
				"jump_serial":   {"g"},
				"reload":        {"R"},
			},
		},

		Devices: nil, // operator-supplied; example carries placeholders only
	}
}
