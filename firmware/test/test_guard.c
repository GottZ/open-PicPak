/* Host unit test for the recovery-guard pure logic (guard_core.h).
 * Compile + run on the host (no ESP hardware). Part of P1 test-first. */
#include "guard_core.h"
#include <stdio.h>

static int fails = 0;
#define CHECK(cond) do { if (!(cond)) { printf("  FAIL: %s\n", #cond); fails++; } } while (0)
#define T GUARD_DEFAULT_THRESHOLD

int main(void)
{
    guard_decision_t d;

    /* 1. Fresh app (reflash/OTA): never inherit a loop -> count resets to 1, no safe mode,
     *    even if the previous count was way over threshold. */
    d = guard_on_boot(99, true, T);
    CHECK(d.new_count == 1);
    CHECK(d.safe_mode == false);

    /* 2. Normal boots below threshold: increment, no safe mode. */
    d = guard_on_boot(0, false, T); CHECK(d.new_count == 1); CHECK(!d.safe_mode);
    d = guard_on_boot(1, false, T); CHECK(d.new_count == 2); CHECK(!d.safe_mode);
    d = guard_on_boot(T - 1, false, T); CHECK(d.new_count == T); CHECK(!d.safe_mode);

    /* 3. A single crash must NOT trip safe mode (negative-probe of the threshold). */
    d = guard_on_boot(0, false, T); CHECK(!d.safe_mode);

    /* 4. Crossing the threshold -> safe mode. */
    d = guard_on_boot(T, false, T); CHECK(d.new_count == T + 1); CHECK(d.safe_mode);

    /* 5. Stays in safe mode while the count is high. */
    d = guard_on_boot(T + 5, false, T); CHECK(d.safe_mode);

    /* 6. Saturation: count never wraps past 255. */
    d = guard_on_boot(255, false, T); CHECK(d.new_count == 255); CHECK(d.safe_mode);

    /* 7. Configurable threshold shifts the trip point both ways. */
    d = guard_on_boot(1, false, 1); CHECK(d.new_count == 2); CHECK(d.safe_mode);    /* thr=1: 2nd bad boot trips */
    d = guard_on_boot(7, false, 8); CHECK(d.new_count == 8); CHECK(!d.safe_mode);   /* thr=8: 8 still ok */
    d = guard_on_boot(8, false, 8); CHECK(d.new_count == 9); CHECK(d.safe_mode);    /* thr=8: 9 trips */

    if (fails == 0) { printf("ALL GUARD CORE TESTS PASSED\n"); return 0; }
    printf("%d CHECK(S) FAILED\n", fails);
    return 1;
}
