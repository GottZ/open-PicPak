/* Host unit test for the setup-console gap decision (console_core.h).
 * Compile + run on the host (no ESP hardware). Design 03-fw-command-ready §4.1:
 * gap = force_open ? ((!forced_setup && cmd_ready) ? grace : 0) : window. */
#include "console_core.h"
#include <stdio.h>

static int fails = 0;
#define CHECK(cond) do { if (!(cond)) { printf("  FAIL: %s\n", #cond); fails++; } } while (0)

#define GRACE  90000
#define WINDOW 3000

int main(void)
{
    /* All 8 (force_open, forced_setup, cmd_ready) combinations.
     * 1. The ONLY self-exit case: setup open for missing config (not operator-forced),
     *    config now locally command-ready -> long bounded grace window. */
    CHECK(console_gap_ms(true,  false, true,  GRACE, WINDOW) == GRACE);

    /* 2.-4. force_open stays disconnect-only (gap 0 = fail-closed, console pinned):
     *    triple-press / safe-mode operator (forced_setup) regardless of readiness,
     *    and any not-yet-command-ready device. */
    CHECK(console_gap_ms(true,  true,  true,  GRACE, WINDOW) == 0);   /* operator keeps console */
    CHECK(console_gap_ms(true,  true,  false, GRACE, WINDOW) == 0);
    CHECK(console_gap_ms(true,  false, false, GRACE, WINDOW) == 0);   /* incomplete config */

    /* 5.-8. !force_open (config existed at entry): the short peek window, independent
     *    of forced_setup/cmd_ready. */
    CHECK(console_gap_ms(false, false, false, GRACE, WINDOW) == WINDOW);
    CHECK(console_gap_ms(false, false, true,  GRACE, WINDOW) == WINDOW);
    CHECK(console_gap_ms(false, true,  false, GRACE, WINDOW) == WINDOW);
    CHECK(console_gap_ms(false, true,  true,  GRACE, WINDOW) == WINDOW);

    if (fails == 0) { printf("ALL CONSOLE CORE TESTS PASSED\n"); return 0; }
    printf("%d CHECK(S) FAILED\n", fails);
    return 1;
}
