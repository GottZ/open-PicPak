/* Host unit test for the low-battery gate pure logic (lowbatt_core.h).
 * Compile + run on the host (no ESP hardware):  cc -I main -O2 -Wall -o /tmp/t test/test_lowbatt.c && /tmp/t
 * Negative-property first: the gate must NOT arm on a single dip / a healthy cell / when disabled. */
#include "lowbatt_core.h"
#include <stdio.h>

static int fails = 0;
#define CHECK(cond) do { if (!(cond)) { printf("  FAIL line %d: %s\n", __LINE__, #cond); fails++; } } while (0)

/* defaults mirrored from main.c (the decision is independent of where they come from) */
static const lowbatt_cfg_t CFG = { .arm_mv = 3300, .clr_mv = 3500, .rise_mv = 40, .arm_streak = 2 };
static const lowbatt_state_t S0 = { .lock = false, .last_mv = -1, .low_streak = 0 };

int main(void)
{
    lowbatt_result_t r;

    /* 1. Disabled gate -> always NORMAL, state untouched, even on a flat-dead cell. */
    r = lowbatt_decide(3000, false, false, S0, CFG);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 0);

    /* 2. Healthy cell -> NORMAL, streak cleared. */
    r = lowbatt_decide(3900, false, true, S0, CFG);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 0);

    /* 3. NEGATIVE PROBE: a single sub-ARM read must NOT arm (transient/brownout reject). */
    r = lowbatt_decide(3200, false, true, S0, CFG);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 1);

    /* 4. Second consecutive low read -> ARM (render once + low-power sleep), baseline captured. */
    lowbatt_state_t s1 = r.next;                       /* streak == 1 from step 3 */
    r = lowbatt_decide(3180, false, true, s1, CFG);
    CHECK(r.action == LOWBATT_ARM); CHECK(r.next.lock); CHECK(r.next.last_mv == 3180);
    CHECK(r.next.low_streak == 0);

    /* 5. A healthy read between the two lows resets the streak (no arming). */
    lowbatt_state_t s_streak1 = lowbatt_decide(3200, false, true, S0, CFG).next; /* streak 1 */
    r = lowbatt_decide(3900, false, true, s_streak1, CFG);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(r.next.low_streak == 0); CHECK(!r.next.lock);

    /* 6. Locked + still sagging -> STAY_LOW, no re-render; high-water mark tracks the max. */
    lowbatt_state_t locked = { .lock = true, .last_mv = 3180, .low_streak = 0 };
    r = lowbatt_decide(3170, false, true, locked, CFG);   /* sagged below baseline */
    CHECK(r.action == LOWBATT_STAY_LOW); CHECK(r.next.lock); CHECK(r.next.last_mv == 3180); /* max kept */
    r = lowbatt_decide(3200, false, true, locked, CFG);   /* small wobble up, < rise(40) */
    CHECK(r.action == LOWBATT_STAY_LOW); CHECK(r.next.lock); CHECK(r.next.last_mv == 3200); /* high-water rises */

    /* 7. RELATIVE recovery: mv up >= rise(40) vs last_mv -> resume NORMAL, unlock. */
    r = lowbatt_decide(3225, false, true, locked, CFG);   /* 3225 - 3180 = 45 >= 40 */
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 0);

    /* 8. ABSOLUTE recovery: at/above CLEAR even without a big single-step rise. */
    lowbatt_state_t lockedHigh = { .lock = true, .last_mv = 3490, .low_streak = 0 };
    r = lowbatt_decide(3500, false, true, lockedHigh, CFG);  /* +10 only, but >= CLEAR */
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock);

    /* 9. Slow charge accumulates via the running max toward CLEAR across cycles. */
    lowbatt_state_t L = { .lock = true, .last_mv = 3300, .low_streak = 0 };
    r = lowbatt_decide(3320, false, true, L, CFG);  /* +20 < rise, < CLEAR -> stay, max=3320 */
    CHECK(r.action == LOWBATT_STAY_LOW); CHECK(r.next.last_mv == 3320);
    r = lowbatt_decide(3340, false, true, r.next, CFG); /* vs 3320: +20 < rise -> stay, max=3340 */
    CHECK(r.action == LOWBATT_STAY_LOW); CHECK(r.next.last_mv == 3340);
    /* a later jump of >= rise vs the (now higher) baseline finally recovers */
    r = lowbatt_decide(3385, false, true, r.next, CFG); /* 3385-3340=45 -> recover */
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock);

    /* 10. BUTTON override while locked -> resume NORMAL regardless of voltage, unlock. */
    r = lowbatt_decide(3100, true, true, locked, CFG);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 0);

    /* 11. Implausible reading (button held / no battery) must NOT arm or unlock a lock. */
    r = lowbatt_decide(-1, false, true, S0, CFG);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock); CHECK(r.next.low_streak == 0);
    r = lowbatt_decide(-1, false, true, locked, CFG);     /* implausible while locked -> stay put */
    CHECK(r.action == LOWBATT_NORMAL); CHECK(r.next.lock == true);  /* state unchanged, no decision */

    /* 12. Button override even with an implausible read (cause is known from the wake, not the pin). */
    r = lowbatt_decide(-1, true, true, locked, CFG);
    CHECK(r.action == LOWBATT_NORMAL); CHECK(!r.next.lock);

    if (fails == 0) { printf("ALL LOWBATT CORE TESTS PASSED\n"); return 0; }
    printf("%d CHECK(S) FAILED\n", fails);
    return 1;
}
