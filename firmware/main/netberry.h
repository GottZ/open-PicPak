#pragma once
#include "berry.h"

/* Register the Berry net surface (wifi_scan/connect/ip/ssid/rssi/stop, http_get) on a VM.
 * Belongs in the policy phase (WiFi up), like fb_register/dev_register/store_register. */
void net_register(bvm *vm);
