/* Host unit test for the low-battery gate pure logic (lowbatt_core.h).
 * Compile + run on the host (no ESP hardware):  cc -I main -O2 -Wall -o /tmp/t test/test_lowbatt.c && /tmp/t
 * Negative-property first: the gate must NOT arm on a single dip / a healthy cell / when disabled /
 * while a USB data host is attached. */
#include "lowbatt_core.h"
#include <stdio.h>

static int fails = 0;
#define CHECK(cond) do { if (!(cond)) { printf("  FAIL line %d: %s\n", __LINE__, #cond); fails++; } } while (0)

/* defaults mirrored from lowbatt.h (the decision is independent of where they come from) */
static const lowbatt_cfg_t CFG = { .arm_mv = 3300, .clr_mv = 3500, .rise_mv = 40, .arm_streak = 2 };
static const lowbatt_state_t S0 = { .lock = false, .last_mv = -1, .low_streak = 0 };

/* dec(mv, button, usb, state) with the gate enabled and the default cfg. */
static lowbatt_result_t dec(int mv, int button, int usb, lowbatt_state_t st)
{
    return lowbatt_decide(mv, button, usb, true, st, CFG);
}

int main(void)
{
    lowbatt_result_t r;

    /* 1. Disabled gate -> always NORMAL, state untouched, even on a flat-dead cell. */
    r = lowbatt_decide(3000, false, false, false, S0, CFG);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 0);

    /* 2. Healthy cell -> NORMAL, streak cleared. */
    r = dec(3900, false, false, S0);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 0);

    /* 3. NEGATIVE PROBE: a single sub-ARM read must NOT arm (transient/brownout reject). */
    r = dec(3200, false, false, S0);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 1);

    /* 4. Second consecutive low read -> ARM (render once + low-power sleep), baseline captured. */
    lowbatt_state_t s1 = r.next;                       /* streak == 1 from step 3 */
    r = dec(3180, false, false, s1);
    CHECK(r.action == LOWBATT_ARM); CHECK(r.next.lock); CHECK(r.next.last_mv == 3180);
    CHECK(r.next.low_streak == 0);

    /* 5. A healthy read between the two lows resets the streak (no arming). */
    lowbatt_state_t s_streak1 = dec(3200, false, false, S0).next; /* streak 1 */
    r = dec(3900, false, false, s_streak1);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(r.next.low_streak == 0); CHECK(!r.next.lock);

    /* 6. Locked + still sagging -> STAY_LOW, no re-render; high-water mark tracks the max. */
    lowbatt_state_t locked = { .lock = true, .last_mv = 3180, .low_streak = 0 };
    r = dec(3170, false, false, locked);                  /* sagged below baseline */
    CHECK(r.action == LOWBATT_STAY_LOW); CHECK(r.next.lock); CHECK(r.next.last_mv == 3180); /* max kept */
    r = dec(3200, false, false, locked);                  /* small wobble up, < rise(40) */
    CHECK(r.action == LOWBATT_STAY_LOW); CHECK(r.next.lock); CHECK(r.next.last_mv == 3200); /* high-water rises */

    /* 7. RELATIVE recovery: mv up >= rise(40) vs last_mv -> resume NORMAL, unlock. */
    r = dec(3225, false, false, locked);                  /* 3225 - 3180 = 45 >= 40 */
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 0);

    /* 8. ABSOLUTE recovery: at/above CLEAR even without a big single-step rise. */
    lowbatt_state_t lockedHigh = { .lock = true, .last_mv = 3490, .low_streak = 0 };
    r = dec(3500, false, false, lockedHigh);              /* +10 only, but >= CLEAR */
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock);

    /* 9. Slow charge accumulates via the running max toward CLEAR across cycles. */
    lowbatt_state_t L = { .lock = true, .last_mv = 3300, .low_streak = 0 };
    r = dec(3320, false, false, L);          /* +20 < rise, < CLEAR -> stay, max=3320 */
    CHECK(r.action == LOWBATT_STAY_LOW); CHECK(r.next.last_mv == 3320);
    r = dec(3340, false, false, r.next);     /* vs 3320: +20 < rise -> stay, max=3340 */
    CHECK(r.action == LOWBATT_STAY_LOW); CHECK(r.next.last_mv == 3340);
    r = dec(3385, false, false, r.next);     /* 3385-3340=45 -> recover */
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock);

    /* 10. BUTTON override while locked -> resume NORMAL regardless of voltage, unlock. */
    r = dec(3100, true, false, locked);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 0);

    /* 11. Implausible reading (button held / no battery) must NOT arm or unlock a lock. */
    r = dec(-1, false, false, S0);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 0);
    r = dec(-1, false, false, locked);       /* implausible while locked -> stay put */
    CHECK(r.action == LOWBATT_NORMAL); CHECK(r.next.lock == true);  /* state unchanged, no decision */

    /* 12. Button override even with an implausible read (cause is known from the wake, not the pin). */
    r = dec(-1, true, false, locked);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock);

    /* 13. USB DATA HOST present -> NEVER arm, even on a flat cell (tethered = not on battery).
     *     This is the soft-lock guard: with a host attached the gate can't lock -> console stays
     *     reachable. (NEGATIVE PROBE of the footgun fix.) */
    r = dec(3000, false, true, S0);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 0);
    r = dec(3000, false, true, s1);          /* streak 1 + low + host -> still NORMAL, no arm */
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock);

    /* 14. USB host appears while LOCKED (battery loop, then plugged into a data host) -> unlock + NORMAL. */
    r = dec(3100, false, true, locked);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 0);

    /* 15. No USB host (battery / dumb 5V charger sends no SOF) -> the gate still operates + arms. */
    r = dec(3180, false, false, s1);         /* streak 1 -> 2, no host -> ARM */
    CHECK(r.action == LOWBATT_ARM); CHECK(r.next.lock);

    if (fails == 0) { printf("ALL LOWBATT CORE TESTS PASSED\n"); return 0; }
    printf("%d CHECK(S) FAILED\n", fails);
    return 1;
}
