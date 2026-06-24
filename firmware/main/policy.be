# Net-phase Berry policy. Runs with WiFi UP, after the autonomous connect + OTA self-verify,
# before net_wifi_stop. The multi-WLAN / rotation policy lives here later; for now it observes
# the live connection and records it via the store (read back over the console: STORE GET net.*),
# which proves the net_* + store_* bindings work in the policy VM and the phase is wired in.
var ip = wifi_ip()
var ssid = wifi_ssid()
var r = wifi_rssi()
print("POLICY ip=" + (ip == nil ? "nil" : ip) + " ssid=" + (ssid == nil ? "nil" : ssid)
      + " rssi=" + str(r))                          # visible in the boot log -> live-net proof
if ip != nil   store_set("net.ip", ip)     end
if ssid != nil store_set("net.ssid", ssid) end
if r != nil    store_set("net.rssi", str(r)) end
# hardening probes: malformed calls must be clean no-ops, never crash the phase
wifi_connect()      # missing args -> false
http_get(42)        # non-string   -> nil
store_set("net.probe", "survived")
print("POLICY probe survived")                      # proves execution continued past the bad calls
