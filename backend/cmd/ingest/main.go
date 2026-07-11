// Command ingest is the public telemetry/OTA/frame ingest endpoint for the open-picpak fleet.
//
// A35-W1 §2: the request-handling logic now lives in internal/ingestcore so the merged cmd/backend can
// mount it in-process. This thin process shim keeps the standalone ingest binary buildable until the
// three legacy cmds are retired at the end of the wave (one-truth). See internal/ingestcore.Main.
package main

import "github.com/open-picpak/backend/internal/ingestcore"

func main() { ingestcore.Main() }
