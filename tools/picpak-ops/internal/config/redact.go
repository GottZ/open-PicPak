package config

import (
	"fmt"
	"hash/fnv"
)

// The redaction contract (owned by config, consumed by every axis). Two classes
// of field are sensitive:
//
//   - Secrets: database.dsn, ota.admin_api_token. Their value (password / bearer
//     token) must never appear anywhere — they render as the config-key NAME.
//   - Host-bearing / private identifiers: the resolved DSN host, hosts[].ssh_target,
//     hosts[].name, ota.admin_api_url, devices[].mac. These carry the exact private
//     hosts/IPs/MACs the air-gap denylist exists to suppress — they render as a
//     stable opaque tag (a config-key reference or a host-keyed hash), never the
//     resolved value.
//
// Every consuming axis renders sensitive fields THROUGH these helpers, never the
// raw value. A log line like "connected to <host>:5432/picpak" would leak the
// private host into the terminal, the scrollback ring, and any pasted bug report.

// RedactSecret renders a secret as its config-key name, never its value. The
// returned tag is stable and value-independent, so two different tokens for the
// same field render identically — no information about the secret leaks.
func RedactSecret(keyName string) string {
	return fmt.Sprintf("«secret:%s»", keyName)
}

// RedactHost renders a host-bearing value as a stable opaque tag keyed by the
// value (so the same host renders identically across log lines, aiding
// correlation) without ever exposing the host/IP/MAC. Empty values render as a
// bare unset tag so an absent host is distinguishable from a present-but-redacted
// one without revealing anything.
func RedactHost(keyName, value string) string {
	if value == "" {
		return fmt.Sprintf("«%s:unset»", keyName)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(value))
	return fmt.Sprintf("«%s#%08x»", keyName, h.Sum32())
}

// RedactedDSN is the contract rendering of database.dsn (a secret AND
// host-bearing field): never the resolved host:port/db, only the key reference.
func (c *Config) RedactedDSN() string {
	return RedactSecret("database.dsn")
}

// RedactedAdminToken is the contract rendering of ota.admin_api_token.
func (c *Config) RedactedAdminToken() string {
	return RedactSecret("ota.admin_api_token")
}

// RedactedAdminURL is the contract rendering of ota.admin_api_url (host-bearing).
func (c *Config) RedactedAdminURL() string {
	return RedactHost("ota.admin_api_url", c.OTA.AdminAPIURL)
}

// RedactedHost renders a hosts[] entry's private fields as opaque tags. The
// returned string carries the host's stable index-independent identity (keyed by
// ssh_target) and never the alias/target itself.
func (c *Config) RedactedHost(h Host) string {
	return RedactHost("hosts.ssh_target", h.SSHTarget)
}

// RedactedDeviceMAC renders a device MAC (a private identifier) as an opaque tag.
func RedactedDeviceMAC(mac string) string {
	return RedactHost("devices.mac", mac)
}

// String on Config renders a fully redacted summary safe to log/print. It NEVER
// includes a resolved secret or host: secrets show their key name, host-bearing
// fields show opaque tags. This is what a panic dump / bug report should carry.
func (c *Config) String() string {
	return fmt.Sprintf(
		"config{schema:%d hosts:%d devices:%d database.dsn:%s ota.admin_api_url:%s ota.admin_api_token:%s db.read_only:%t ota.allow_direct_write:%t}",
		c.Schema,
		len(c.Hosts),
		len(c.Devices),
		c.RedactedDSN(),
		c.RedactedAdminURL(),
		c.RedactedAdminToken(),
		c.Database.ReadOnly,
		c.OTA.AllowDirectWrite,
	)
}
