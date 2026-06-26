package config

// envOverride is one enumerated environment override. The surface is explicit
// (not "any key as env") so it is auditable: exactly these three variables can
// override exactly these fields, and nothing else.
type envOverride struct {
	env   string                // environment variable name
	field string                // human field path (for errors / redaction key names)
	apply func(*Config, string) // write the resolved value into the config
}

// envOverrides is the auditable env-override table. Secrets (DSN password, admin
// token) MUST be supplyable via env so a gitignored file can omit them on shared
// machines.
func envOverrides() []envOverride {
	return []envOverride{
		{
			env:   "PICPAK_OPS_DB_DSN",
			field: "database.dsn",
			apply: func(c *Config, v string) { c.Database.DSN = v },
		},
		{
			env:   "PICPAK_OPS_ADMIN_URL",
			field: "ota.admin_api_url",
			apply: func(c *Config, v string) { c.OTA.AdminAPIURL = v },
		},
		{
			env:   "PICPAK_OPS_ADMIN_TOKEN",
			field: "ota.admin_api_token",
			apply: func(c *Config, v string) { c.OTA.AdminAPIToken = v },
		},
	}
}

// applyEnv overlays the enumerated env variables onto c. An unset/empty variable
// leaves the field as merged from the file (file < env: a set env always wins).
func applyEnv(c *Config, getenv func(string) string) {
	for _, o := range envOverrides() {
		if v := getenv(o.env); v != "" {
			o.apply(c, v)
		}
	}
}
