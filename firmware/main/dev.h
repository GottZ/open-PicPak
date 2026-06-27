/* Berry-native device-identity module: mac(), serial(), color(). */
#pragma once
#include "berry.h"
void dev_register(bvm *vm);
void dev_dump_nvs(void);        /* diagnostic: print every NVS entry (ns/key/type/value) */
void dev_measure_battery(void); /* call FIRST after wake, before WiFi/EPD/Berry load the rail */
int  dev_batt_mv(void);         /* battery mV from the last measure; <0 = unset/implausible (C-side gate) */
int  dev_batt_pct(void);        /* battery percent from the last measure; <0 = unset/implausible */
