// Command faas-supervisor is the TRUSTED FaaS process (holds SECRETS_KEY + the DB).
//
// A35-W1 §2: the supervisor logic now lives in internal/faascore so the merged cmd/backend can drive
// it in-process (no render.sock / test.sock transport). This thin process shim keeps the standalone
// binary buildable until the legacy cmds are retired in step 4 (one-truth). See internal/faascore.Main.
package main

import "github.com/open-picpak/backend/internal/faascore"

func main() { faascore.Main() }
