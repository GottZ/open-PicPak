/* Berry WASM simulator wrapper (render profile, minimal) — A33 W2.
 *
 * Compiles the REAL pinned Berry VM (pin bd9c93b = Berry 1.1.0) + the REAL
 * main/fb.c so the browser renders byte-identical frames to firmware/host/
 * host_render.c on a PC (see design/33 §2.1, §4.3). Device identity is stubbed
 * exactly like host_render.c; the dev_* values are format-preserving placeholders.
 *
 * Two hard differences to host_render.c, both load-bearing for the sim:
 *   1. Module deactivation is REAL, via the generated sim/berry_conf.h (gen-conf.sh),
 *      NOT a -D override (the header defines the switches unconditionally, so -D is a
 *      no-op — design/33 §2.1). FILE_SYSTEM/OS/SYS/SHARED_LIB are 0 → `import os`
 *      raises instead of running.
 *   2. A wall-clock deadline (100 ms) is enforced from the VM observability hook
 *      (be_set_obs_hook + BE_OBS_VM_HEARTBEAT), so `while true end` aborts as rc=3
 *      instead of hanging the Worker (design/33 §4.2; hosted in a Web Worker per
 *      board decision E-A33-2, so the 100 ms is a Worker-local budget).
 *
 * The wrapper API is driven from JS via ccall/cwrap (see backend/web/src/lib/sim/).
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <emscripten.h>
#include "berry.h"
#include "fb.h"

#define SIM_DEADLINE_MS 100.0

static bvm   *g_vm;
static double g_start;          /* emscripten_get_now() at run start */
static int    g_deadline_hit;   /* set by the obshook before it raises */
static char   g_err[512];       /* last error text ("type: message") */

/* Overridable stub device state (defaults mirror host_render.c). sim_set_dev(json)
 * patches these; the render profile does not otherwise exercise them, but keeping
 * them real means C2/later waves inherit an honest surface. */
static int    g_batt_mv  = 3900;
static int    g_batt_pct = 70;
static int    g_uptime_ms = 1234;

/* --- stub device identity (format-preserving; on-device: esp_read_mac()/NVS) --- */
static int h_mac(bvm *vm)     { be_pushstring(vm, "02:00:00:00:00:01"); be_return(vm); }
static int h_bt_mac(bvm *vm)  { be_pushstring(vm, "02:00:00:00:00:02"); be_return(vm); }
static int h_chip(bvm *vm)    { be_pushstring(vm, "ESP32-C3");          be_return(vm); }
static int h_reset(bvm *vm)   { be_pushstring(vm, "usb");               be_return(vm); }
static int h_uptime(bvm *vm)  { be_pushint(vm, g_uptime_ms);            be_return(vm); }
static int h_batt_mv(bvm *vm) { be_pushint(vm, g_batt_mv);              be_return(vm); }
static int h_batt_pct(bvm *vm){ be_pushint(vm, g_batt_pct);            be_return(vm); }
static int h_nvs(bvm *vm)     { (void)vm; be_return_nil(vm); }          /* no NVS on host */

/* Deadline enforcement. The hook fires on every VM heartbeat (~2^20 instructions,
 * measured 2-6 ms in WASM, design/33 §2.1). Once the wall clock crosses the budget
 * we raise: be_raise longjmps back to the enclosing be_pcall, which needs
 * -sSUPPORT_LONGJMP=emscripten. Real render scripts finish far under one heartbeat. */
static void obs_hook(bvm *vm, int event, ...)
{
    if (event == BE_OBS_VM_HEARTBEAT) {
        if (emscripten_get_now() - g_start > SIM_DEADLINE_MS) {
            g_deadline_hit = 1;
            be_raise(vm, "deadline_error", "script exceeded the 100 ms simulator budget");
        }
    }
}

static void register_stubs(bvm *vm)
{
    fb_register(vm);   /* the REAL fb.c: fill/pixel/rect/disc/circle/triangle/text/text16/qr/dump */
    be_regfunc(vm, "dev_mac",       h_mac);
    be_regfunc(vm, "dev_bt_mac",    h_bt_mac);
    be_regfunc(vm, "nvs_str",       h_nvs);
    be_regfunc(vm, "dev_chip",      h_chip);
    be_regfunc(vm, "dev_uptime_ms", h_uptime);
    be_regfunc(vm, "dev_reset",     h_reset);
    be_regfunc(vm, "dev_batt_mv",   h_batt_mv);
    be_regfunc(vm, "dev_batt_pct",  h_batt_pct);
}

/* Capture the two-value error state Berry leaves on the stack after a failed
 * be_loadstring/be_pcall: [-2]=exception type, [-1]=message. */
static void capture_error(bvm *vm)
{
    const char *etype = be_isstring(vm, -2) ? be_tostring(vm, -2) : "error";
    const char *emsg  = be_tostring(vm, -1);
    snprintf(g_err, sizeof g_err, "%s: %s", etype ? etype : "error", emsg ? emsg : "");
    be_pop(vm, 2);
}

/* Minimal integer-field extractor for sim_set_dev(json) — avoids pulling a JSON
 * parser into the wrapper. Recognises "key": <int>. */
static int json_int(const char *json, const char *key, int fallback)
{
    char pat[64];
    snprintf(pat, sizeof pat, "\"%s\"", key);
    const char *p = strstr(json, pat);
    if (!p) return fallback;
    p += strlen(pat);
    while (*p && *p != ':') p++;
    if (*p != ':') return fallback;
    p++;
    while (*p == ' ' || *p == '\t') p++;
    int sign = 1;
    if (*p == '-') { sign = -1; p++; }
    if (*p < '0' || *p > '9') return fallback;
    long v = 0;
    while (*p >= '0' && *p <= '9') { v = v * 10 + (*p - '0'); p++; }
    return sign * (int)v;
}

/* ------------------------------- wrapper API ------------------------------- */

EMSCRIPTEN_KEEPALIVE
void sim_reset(int profile)
{
    (void)profile;   /* render profile only in W2; C2 lands in W4 */
    if (g_vm) be_vm_delete(g_vm);
    g_vm = be_vm_new();
    be_set_obs_hook(g_vm, obs_hook);
    register_stubs(g_vm);
    g_err[0] = 0;
}

EMSCRIPTEN_KEEPALIVE
void sim_set_dev(const char *json)
{
    if (!json) return;
    g_batt_mv   = json_int(json, "batt_mv",   g_batt_mv);
    g_batt_pct  = json_int(json, "batt_pct",  g_batt_pct);
    g_uptime_ms = json_int(json, "uptime_ms", g_uptime_ms);
}

/* 0=ok 1=compile-err 2=runtime-err 3=deadline */
EMSCRIPTEN_KEEPALIVE
int sim_run(const char *source)
{
    if (!g_vm) sim_reset(0);
    g_err[0] = 0;
    g_deadline_hit = 0;

    int r = be_loadstring(g_vm, source);
    if (r != BE_OK) { capture_error(g_vm); return 1; }

    g_start = emscripten_get_now();
    r = be_pcall(g_vm, 0);
    if (r != BE_OK) {
        int deadline = g_deadline_hit;
        capture_error(g_vm);
        return deadline ? 3 : 2;
    }
    be_pop(g_vm, 1);   /* drop the return value of the top-level call */
    return 0;
}

/* Syntax check without execution (design/33 §4.4, third linter() source). */
EMSCRIPTEN_KEEPALIVE
int sim_compile_only(const char *source)
{
    if (!g_vm) sim_reset(0);
    g_err[0] = 0;
    int r = be_loadstring(g_vm, source);
    if (r != BE_OK) { capture_error(g_vm); return 1; }
    be_pop(g_vm, 1);   /* drop the compiled closure, never run it */
    return 0;
}

EMSCRIPTEN_KEEPALIVE
const char *sim_error(void) { return g_err; }

EMSCRIPTEN_KEEPALIVE
const uint8_t *sim_fb(void) { return fb_buffer(); }   /* 30000 bytes */
