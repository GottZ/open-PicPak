/* Host probe for the A32 W2 present-gate hardening (present_core.h).
 *
 * Proves the gate core in isolation with a REAL Berry VM producing the render_ok signal
 * the exact same way berry_render() does (main.c:194-206): be_loadstring -> be_pcall,
 * ok == (result == BE_OK). A Berry runtime error must translate into a "no present"
 * decision; a clean script into "present". The actual EPD present stays ESP-woven; only
 * the validity-flag logic is exercised here (design 32-firmware-frame-path.md W-A32.2).
 *
 * Two gate models are selectable via argv[1] so the SAME source shows red then green:
 *   "ist"   -> the current firmware: present UNCONDITIONALLY (models main.c:345 today).
 *              The broken-script case still "presents" -> the blank bug -> test FAILS (RED).
 *   "fixed" -> present_gate_allows(render_ok) from present_core.h.
 *              Broken -> no present, valid -> present -> test PASSES (GREEN).
 * The "fixed" run doubles as the regression guard for the fix.
 *
 * Build + run (from firmware/, needs the Berry component via scripts/setup-berry.sh):
 *   gcc -O1 -w -I main -I components/berry/src -I components/berry/generate -I components/berry \
 *       test/test_present.c \
 *       components/berry/src/*.c components/berry/port/be_port.c components/berry/port/be_modtab.c \
 *       -o build/test_present -lm
 *   ./build/test_present ist     # expect RED (demonstrates the Ist-Code blank bug)
 *   ./build/test_present fixed    # expect GREEN (the W2 fix)
 */
#include "present_core.h"
#include "berry.h"
#include <stdio.h>
#include <string.h>

static int fails = 0;
#define CHECK(cond) do { if (!(cond)) { printf("  FAIL line %d: %s\n", __LINE__, #cond); fails++; } } while (0)

/* Mirror of berry_render()'s success contract (main.c:194-206): true iff the script both
 * compiles and runs without raising. No fb/dev surface registered -> the probe scripts must
 * not touch the device stdlib; render_ok here is purely the compile+run outcome. */
static bool render_ok_of(const char *script)
{
    bvm *vm = be_vm_new();
    int r = be_loadstring(vm, script);
    if (r == BE_OK) r = be_pcall(vm, 0);
    be_vm_delete(vm);
    return r == BE_OK;
}

/* The two gate models, selected at runtime so one source yields red (ist) and green (fixed). */
static bool decide_present(const char *mode, bool render_ok)
{
    if (strcmp(mode, "ist") == 0) return true;         /* unconditional present (main.c:345 today) */
    return present_gate_allows(render_ok);             /* the W2 hardening */
}

int main(int argc, char **argv)
{
    const char *mode = (argc > 1) ? argv[1] : "fixed";

    /* A clean script -> berry_render() would return true. */
    const char *ok_script = "var a = 1 + 1\n";
    /* A runtime error (assert raises) -> berry_render() returns false, framebuffer left empty. */
    const char *bad_runtime = "assert(false, 'forced render failure')\n";
    /* A syntax error -> be_loadstring fails before pcall -> also false. */
    const char *bad_syntax = "var = = =\n";

    bool ok_valid  = render_ok_of(ok_script);
    bool ok_rterr  = render_ok_of(bad_runtime);
    bool ok_synerr = render_ok_of(bad_syntax);

    /* Sanity: the Berry VM really distinguishes valid from broken (the probe's premise). */
    CHECK(ok_valid == true);
    CHECK(ok_rterr == false);
    CHECK(ok_synerr == false);

    /* The property under test: present iff the render produced a valid frame. */
    printf("gate mode = %s\n", mode);
    printf("  valid script    -> render_ok=%d  present=%d\n", ok_valid,  decide_present(mode, ok_valid));
    printf("  runtime-err      -> render_ok=%d  present=%d\n", ok_rterr,  decide_present(mode, ok_rterr));
    printf("  syntax-err       -> render_ok=%d  present=%d\n", ok_synerr, decide_present(mode, ok_synerr));

    CHECK(decide_present(mode, ok_valid)  == true);    /* valid render must present */
    CHECK(decide_present(mode, ok_rterr)  == false);   /* runtime error -> NO present (no blank) */
    CHECK(decide_present(mode, ok_synerr) == false);   /* syntax error  -> NO present (no blank) */

    if (fails == 0) { printf("ALL PRESENT-GATE PROBES PASSED (%s)\n", mode); return 0; }
    printf("%d CHECK(S) FAILED (%s)\n", fails, mode);
    return 1;
}
