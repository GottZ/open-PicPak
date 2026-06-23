#ifndef GUARD_H
#define GUARD_H
#include <stdbool.h>
#include <stdint.h>
/* Recovery guard: bootloop detection -> USB-reachable safe mode.
 * See open-picpak .project/design/01-recovery-guard.md. */

/* Call EARLY in app_main, right after nvs_flash_init (before WiFi/EPD). Increments the
 * persisted bad-boot counter (reset to 1 if the app is fresh = elf-sha changed). If the
 * count exceeds the threshold, enters safe mode and NEVER RETURNS. Otherwise it starts
 * the stable-uptime watchdog task and returns so the normal boot continues. */
void guard_check_and_run(void);

/* Mark the firmware stable -> bad-boot counter = 0. Call on a controlled, healthy point
 * (e.g. entry into enter_deep_sleep). Cheap, idempotent. */
void guard_mark_stable(void);

/* Console helpers (GUARD command). */
unsigned guard_bad_boots(void);   /* current persisted bad-boot count */
void     guard_clear(void);       /* bad_boots = 0 (recovery from the console) */

/* Tunable thresholds: compile-time defaults (GUARD_DEFAULT_THRESHOLD / 20000 ms), overridable
 * at runtime via NVS. > threshold consecutive un-stable boots -> safe mode; a boot is "stable"
 * after stable-uptime-ms of uninterrupted runtime. */
unsigned guard_threshold(void);
unsigned guard_stable_ms(void);
void     guard_set_threshold(unsigned v);   /* clamped < 255 */
void     guard_set_stable_ms(unsigned v);

#endif /* GUARD_H */
