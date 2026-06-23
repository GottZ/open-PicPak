/* Status LED on the button (GPIO21, active-high).
 * State model: deep sleep = off, awake = on, transfer (WiFi/HTTP) = blinking. */
#pragma once

void led_init(void);          /* GPIO21 as output, LED initially off */
void led_on(void);            /* steady on (blink timer is stopped) */
void led_off(void);           /* off (blink timer is stopped) */
void led_blink_start(void);   /* periodic blinking (transfer indicator) */
void led_blink_stop(void);    /* stop blinking (set state afterwards via on/off) */
