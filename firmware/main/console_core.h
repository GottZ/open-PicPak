#ifndef CONSOLE_CORE_H
#define CONSOLE_CORE_H
/* Setup-console gap decision — pure, no hardware/NVS, host-unit-testable
 * (test/test_console.c). See design 03-fw-command-ready §4.1. */
#include <stdbool.h>

/* Between-command first-char timeout for console_run's read_line:
 *  - !force_open (config existed at entry): short window_ms peek, then normal run.
 *  - force_open + forced_setup (triple-press operator / safe mode): 0 = stay open
 *    while a USB host is attached, give up only on disconnect. Fail-closed: the
 *    operator keeps the console, no auto-exit churn (design 03 §5 B2/B3).
 *  - force_open + !forced_setup (setup was open only for missing config): once the
 *    device is locally command-ready (cmd_ready_local), a LONG bounded grace window
 *    -> autonomous self-exit into the normal run; not ready -> 0, console pinned. */
static inline int console_gap_ms(bool force_open, bool forced_setup, bool cmd_ready,
                                 int grace_ms, int window_ms)
{
    if (!force_open) return window_ms;
    return (!forced_setup && cmd_ready) ? grace_ms : 0;
}

#endif /* CONSOLE_CORE_H */
