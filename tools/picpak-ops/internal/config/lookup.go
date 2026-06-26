package config

// Typed O(1) accessors built once at freeze (load.go calls buildIndexes). A
// frozen *Config is never mutated, so these maps are safe to read concurrently
// without locking; hot-reload produces a NEW *Config with its own indexes.

// indexes holds the lookup maps. It is a separate unexported field so the public
// schema in config.go stays a clean TOML shape (the indexes are not decoded).
type indexes struct {
	hostByName     map[string]*Host
	deviceBySerial map[string]*Device
	devicesByHost  map[string][]*Device
}

// idx is the lazily-built index set; nil until buildIndexes runs at freeze.
// (It is a pointer so a zero Config has no indexes and the accessors rebuild
// defensively if used before freeze.)
func (c *Config) buildIndexes() {
	ix := &indexes{
		hostByName:     make(map[string]*Host, len(c.Hosts)),
		deviceBySerial: make(map[string]*Device, len(c.Devices)),
		devicesByHost:  make(map[string][]*Device),
	}
	for i := range c.Hosts {
		h := &c.Hosts[i]
		if h.Name != "" {
			ix.hostByName[h.Name] = h
		}
	}
	for i := range c.Devices {
		d := &c.Devices[i]
		if d.Serial != "" {
			ix.deviceBySerial[d.Serial] = d
		}
		if d.Host != "" {
			ix.devicesByHost[d.Host] = append(ix.devicesByHost[d.Host], d)
		}
	}
	c.idx = ix
}

// ensureIndexes builds the indexes on first use if freeze() was not called (e.g.
// a Config constructed in a test without Load). Safe for read paths because a
// frozen Config is immutable.
func (c *Config) ensureIndexes() *indexes {
	if c.idx == nil {
		c.buildIndexes()
	}
	return c.idx
}

// HostByName returns the host with the given name and whether it exists.
func (c *Config) HostByName(name string) (*Host, bool) {
	h, ok := c.ensureIndexes().hostByName[name]
	return h, ok
}

// DeviceBySerial returns the device with the given serial and whether it exists.
func (c *Config) DeviceBySerial(serial string) (*Device, bool) {
	d, ok := c.ensureIndexes().deviceBySerial[serial]
	return d, ok
}

// DevicesByHost returns the devices normally tethered to the named host. The
// returned slice is the index's own backing slice — callers must not mutate it
// (a frozen Config is read-only).
func (c *Config) DevicesByHost(host string) []*Device {
	return c.ensureIndexes().devicesByHost[host]
}
