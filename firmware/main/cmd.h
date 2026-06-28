/* Berry C2 command surface + executor (Doc 13 Wave 3a).
 *
 * berry_c2() runs a Berry script (from the console C2 RUN verb now, the C2 poll later) in a VM whose
 * stdlib is the device's command surface: a SAFE subset of the console actions (config/NVS writes +
 * read-only queries) plus the store/rtc/dev surfaces. Control-flow verbs (reboot/refresh/sleep) do
 * NOT act in place -- they set an intent the caller actions AFTER persisting any C2 ack, so a reboot
 * command can never loop. The net write surface (wifi connect/stop) and fb (drawing) are deliberately
 * NOT registered. */
#pragma once
#include "berry.h"
#include <stdint.h>
#include <stdbool.h>

typedef enum {
    CMD_INTENT_NONE = 0,
    CMD_INTENT_REFRESH,   /* run a render+display cycle now */
    CMD_INTENT_REBOOT,    /* esp_restart */
    CMD_INTENT_SLEEP,     /* deep sleep for the carried seconds (clamped) */
} cmd_intent_t;

/* Register the C2 command native functions on vm (called by berry_c2). */
void cmd_register(bvm *vm);

/* Run a NUL-terminated Berry script with the full C2 surface (cmd + store + rtc + dev). Resets the
 * intent first; returns the post-run intent (NONE if the script set none) and, via sleep_s, the
 * SLEEP duration. *ok_out (optional) = the script ran without a Berry fault. Best-effort: a fault
 * only logs (does not crash -- be_pcall is protected). */
cmd_intent_t berry_c2(const char *script, uint32_t *sleep_s, bool *ok_out);

/* C2 poll (Wave 3b). c2_poll(): fetch the c2_url (NVS) over HTTPS and run the returned Berry script;
 * returns the post-run intent (caller actions it). Refuses a non-https c2_url. poll_period (NVS, 0=off)
 * is the keep-awake poll cadence. Config setters persist to NVS. */
cmd_intent_t c2_poll(uint32_t *sleep_s, bool *ran);
uint32_t     c2_poll_period(void);
bool         c2_set_url(const char *url);
bool         c2_set_period(uint32_t secs);

/* C2 long-term identity + session: bonding (the ECDSA keypair) lives in c2key.h; the re-key handshake
 * + session HOTP poll are internal to cmd.c. The legacy bc-composite auth (c2_compute_auth /
 * c2_set_secret) was removed at the Doc-15 cutover. */
