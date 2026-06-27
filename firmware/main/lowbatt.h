#pragma once
/* Smart low-battery gate — hardware/NVS glue around the pure FSM in lowbatt_core.h.
 * Loads NVS-overridable thresholds (Policy=Daten, like net_tx_qdbm_load), holds the cross-wake
 * state in RTC-RAM, and exposes the BATT console surface. See design/14-low-battery-gate.md. */
#include <stdbool.h>
#include <stdint.h>
#include "esp_sleep.h"
#include "lowbatt_core.h"

/* Compile-time master switch. 0 (default) -> the gate is governed by the per-device NVS flag
 * `picpak/lb_on` (default 0 too) so the shipped build has ZERO behaviour change until someone
 * runs `BATT ON`. Set to 1 to force the gate on in a build regardless of the flag. */
#ifndef LOWBATT_GATE_ENABLED
#define LOWBATT_GATE_ENABLED 0
#endif

/* Defaults (NVS-overridable via BATT, clamped on load). Derived from the user's "~10%" target
 * + the stock RE hysteresis (ctx 019f08ee), mapped through our LUT (dev.c: 10%/100mV @3200=0). */
#define LOWBATT_ARM_MV_DEF   3300   /* ~10% */
#define LOWBATT_CLR_MV_DEF   3500   /* ~30%, big hysteresis */
#define LOWBATT_RISE_MV_DEF  40     /* "charging" = mV up >= this over a 15-min idle window */
#define LOWBATT_STREAK_DEF   2      /* consecutive low reads before arming */
#define LOWBATT_WAKE_S_DEF   900    /* 15-min low-power poll */

/* Clamp ranges. ARM spans the whole plausible cell window so the on-device probe can force-arm
 * (BATT ARM <above-cell>); CLEAR is additionally floored to ARM+100 at load so it can't invert. */
#define LOWBATT_ARM_MV_MIN   3000
#define LOWBATT_ARM_MV_MAX   4250
#define LOWBATT_CLR_MV_MAX   4250
#define LOWBATT_RISE_MV_MIN  10
#define LOWBATT_RISE_MV_MAX  300
#define LOWBATT_STREAK_MIN   1
#define LOWBATT_STREAK_MAX   5
#define LOWBATT_WAKE_S_MIN   30
#define LOWBATT_WAKE_S_MAX   7200

/* Run the gate for this wake: load cfg + RTC state, decide, persist the next state. The caller
 * acts on the action: ARM -> render the charge screen once + enter_deep_sleep(lowbatt_wake_s());
 * STAY_LOW -> enter_deep_sleep(lowbatt_wake_s()) without re-render; NORMAL -> fall through. */
lowbatt_action_t lowbatt_gate(int batt_mv, esp_sleep_wakeup_cause_t cause);

uint32_t lowbatt_wake_s(void);     /* low-power poll interval (NVS picpak/lb_wake_s, clamped) */
bool     lowbatt_is_enabled(void);
bool     lowbatt_is_locked(void);  /* current RTC lock state (for the render-screen flag) */

/* BATT console surface: per-device tuning + the on-device probe. Each clamps + persists to NVS. */
void lowbatt_status(void);
void lowbatt_set_arm(int mv);
void lowbatt_set_clear(int mv);
void lowbatt_set_rise(int mv);
void lowbatt_set_wake(int s);
void lowbatt_set_streak(int n);
void lowbatt_set_enabled(bool on);
