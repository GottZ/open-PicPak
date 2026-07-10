# builtin/wifi-preset — a C2 config snippet (cannot draw): stage a Wi-Fi credential on the device via
# the C2 safe subset. {{ssid}} / {{pass}} are substituted at apply time (W6). Rides the 8191-byte cap
# (K3 poison-pill) and the HTTPS+HOTP delivery chain like every enqueued script.
set_wifi("{{ssid}}", "{{pass}}")
