#ifndef LOWBATT_CORE_H
#define LOWBATT_CORE_H
/* Smart low-battery gate — pure decision FSM, no hardware/NVS, host-unit-testable.
 * See open-picpak design/14-low-battery-gate.md.
 *
 * Idea: below ARM the device enters a SHORT-timer deep sleep (~15 min) instead of a normal
 * cycle; on each timer wake it re-measures and stays asleep unless the voltage has RISEN
 * (external power attached -> charging) or crossed the higher CLEAR threshold. This detects
 * "power attached" without any VBUS hardware (the C3 has none). State lives in RTC-RAM
 * (nulled on cold boot -> always boots NORMAL); the decision here is pure. */
#include <stdint.h>
#include <stdbool.h>

/* Thresholds (Policy=Daten: NVS-overridable + clamped in lowbatt.c, defaults in main.c). */
typedef struct {
    uint16_t arm_mv;      /* arm below this battery mV */
    uint16_t clr_mv;      /* absolute recovery: at/above this -> resume normal */
    uint16_t rise_mv;     /* relative recovery: mV up >= this vs last sample -> charging */
    uint8_t  arm_streak;  /* consecutive low reads required before arming (transient reject) */
} lowbatt_cfg_t;

/* Cross-wake state, persisted in RTC-RAM by the caller (nulled on cold boot). */
typedef struct {
    bool    lock;        /* in low-power poll mode */
    int16_t last_mv;     /* baseline for the rise check (running max while locked); <0 = unset */
    uint8_t low_streak;  /* consecutive sub-ARM reads while not yet locked */
} lowbatt_state_t;

typedef enum {
    LOWBATT_NORMAL = 0,  /* run the normal cycle (fall through, unchanged behaviour) */
    LOWBATT_ARM,         /* just locked: render the low-battery screen ONCE, then low-power sleep */
    LOWBATT_STAY_LOW,    /* already locked, not recovered: low-power sleep, no re-render */
} lowbatt_action_t;

typedef struct {
    lowbatt_action_t action;
    lowbatt_state_t  next;   /* state to persist for the next wake */
} lowbatt_result_t;

/* Pure decision for one wake.
 *   batt_mv     : current battery mV; < 0 means implausible (no battery / button held) -> never gate.
 *   button_wake : this wake was caused by the GPIO/button (deliberate "resume" press).
 *   usb_present : a USB DATA host (SOF) is attached -> the device is tethered/powered, NOT on
 *                 battery, so it must NOT low-power-gate (it stays reachable via keep-awake). This
 *                 also makes the gate impossible to soft-lock on a tether: with a host present it
 *                 never arms, so the console stays reachable. A dumb 5V charger sends no SOF ->
 *                 usb_present=false -> the gate still operates and detects charge via voltage rise.
 *   enabled     : the gate enable flag (compile + NVS). Off -> always NORMAL, state untouched.
 * Pure: no side effects; the caller persists `next` and acts on `action`. */
static inline lowbatt_result_t lowbatt_decide(int batt_mv, bool button_wake, bool usb_present,
                                              bool enabled, lowbatt_state_t st, lowbatt_cfg_t cfg)
{
    lowbatt_result_t r;
    r.next = st;
    r.action = LOWBATT_NORMAL;

    if (!enabled) return r;                      /* gate off -> behave exactly as today */

    if (usb_present || button_wake) {            /* tethered to a data host, or a deliberate resume */
        r.next.lock = false;                     /* -> never low-power-gate; re-arm only later, on */
        r.next.low_streak = 0;                   /*    battery, after the host/charger is gone */
        if (batt_mv >= 0) r.next.last_mv = (int16_t)batt_mv;
        return r;                                /* NORMAL */
    }

    if (batt_mv < 0) return r;                    /* implausible reading -> don't decide on garbage */

    if (st.lock) {
        /* Low-power poll. Recovery = absolute OR relative-rise. last_mv is a running max so a
         * slow multi-cycle charge accumulates toward CLEAR even if any single step is < rise. */
        bool recovered = (batt_mv >= cfg.clr_mv)
                      || (st.last_mv >= 0 && (batt_mv - (int)st.last_mv) >= (int)cfg.rise_mv);
        if (recovered) {
            r.next.lock = false;
            r.next.low_streak = 0;
            r.next.last_mv = (int16_t)batt_mv;
            r.action = LOWBATT_NORMAL;
        } else {
            if (batt_mv > st.last_mv) r.next.last_mv = (int16_t)batt_mv;   /* track the high-water mark */
            r.action = LOWBATT_STAY_LOW;
        }
        return r;
    }

    /* Not locked. */
    if (batt_mv >= (int)cfg.arm_mv) {
        r.next.low_streak = 0;                    /* healthy read clears the streak */
        return r;                                 /* NORMAL */
    }
    /* Below ARM: count consecutive lows; arm only after the streak (rejects a single noisy dip
     * and a load brownout — though the caller samples pre-load, this is a second guard). */
    uint8_t s = (st.low_streak < 255) ? (uint8_t)(st.low_streak + 1) : 255;
    r.next.low_streak = s;
    if (s >= cfg.arm_streak) {
        r.next.lock = true;
        r.next.last_mv = (int16_t)batt_mv;        /* baseline for the rise check */
        r.next.low_streak = 0;                    /* lock now governs; reset the streak */
        r.action = LOWBATT_ARM;
    }
    return r;                                     /* streak not yet reached -> NORMAL (transient guard) */
}

#endif /* LOWBATT_CORE_H */
