#ifndef GUARD_CORE_H
#define GUARD_CORE_H
/* Recovery-guard pure decision logic — no hardware/NVS, host-unit-testable.
 * See open-picpak design/01-recovery-guard.md + validation/01-recovery-guard.md. */
#include <stdint.h>
#include <stdbool.h>

#define GUARD_DEFAULT_THRESHOLD 4   /* default; runtime-overridable via NVS (see guard.c) */

typedef struct {
    uint8_t new_count;   /* value to persist to NVS for this boot */
    bool    safe_mode;   /* enter USB-only safe mode instead of normal boot */
} guard_decision_t;

/* Decide at boot, given the bad-boot count persisted BEFORE this boot, whether the running
 * app is fresh (elf-sha changed since last seen), and the (runtime-configurable) threshold:
 * > threshold consecutive un-stable boots -> safe mode. Pure: no side effects. */
static inline guard_decision_t guard_on_boot(uint8_t prev_count, bool fresh_app, uint8_t threshold)
{
    guard_decision_t d;
    if (fresh_app) {
        /* Freshly flashed / OTA'd: clean slate, never inherit a previous loop. */
        d.new_count = 1;
        d.safe_mode = false;
        return d;
    }
    uint16_t c = (uint16_t)prev_count + 1u;   /* this boot counts */
    if (c > 255u) c = 255u;                    /* saturate, never wrap */
    d.new_count = (uint8_t)c;
    d.safe_mode = (d.new_count > threshold);
    return d;
}

#endif /* GUARD_CORE_H */
