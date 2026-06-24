/* Berry-native device-identity module: mac(), serial(), color(). */
#pragma once
#include "berry.h"
void dev_register(bvm *vm);
void dev_dump_nvs(void);        /* diagnostic: print every NVS entry (ns/key/type/value) */
void dev_measure_battery(void); /* call FIRST after wake, before WiFi/EPD/Berry load the rail */
