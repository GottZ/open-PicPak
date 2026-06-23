/* Minimal USB-Serial-JTAG console for provisioning.
 * Commands: SETWIFI <ssid> <pass>, SETURL <url>, INFO, REFRESH, SLEEP, ERASE, HELP.
 * Called ONLY on a real boot/reset (not on timer wake). */
#pragma once
#include <stdint.h>
#include <stdbool.h>

typedef enum {
    CONSOLE_PROCEED = 0,   /* window elapsed / no setup -> normal run (fetch) */
    CONSOLE_REFRESH,       /* user: fetch image now + sleep */
    CONSOLE_SLEEP,         /* user: sleep directly without fetch */
    CONSOLE_PRESS,         /* test: N simulated button presses (run_cycles); N via sleep_secs_out */
} console_action_t;

/* Opens the console.
 * force_open: keep a long setup window (triple-press OR no config) so the web tool /
 *   CLI can provision; otherwise (config present) only a brief peek window, then a
 *   normal run.
 * boot_count only for the INFO output.
 * sleep_secs_out: on return CONSOLE_SLEEP the sleep duration requested via "SLEEP <s>"
 * (0 = not specified -> caller takes its default). */
console_action_t console_run(uint32_t boot_count, uint32_t *sleep_secs_out, bool force_open);
