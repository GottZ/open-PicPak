/* present_core.h -- A32 W2: the present-gate decision as pure, host-testable logic.
 *
 * Blank-bug context (design/32-firmware-frame-path.md, Naht B): s_fb is a plain .bss
 * static (fb.c:16), zeroed on every deep-sleep wake -- only s_fb_hash survives in RTC.
 * The field cycle used to call display_framebuffer_if_changed() UNCONDITIONALLY after
 * berry_render() (main.c:345), i.e. even when the render failed and left the framebuffer
 * empty. An all-zero framebuffer hashes differently from the last displayed frame, so the
 * content gate fires and pushes an all-black frame onto the panel (palette 0 = BLACK) ->
 * the latent blank bug.
 *
 * The hardening: present ONLY when the frame producer reports a valid, fully-populated
 * framebuffer. On a producer failure, keep the last frame (E-paper is bistable, the last
 * frame physically survives) and do NOT present. This predicate isolates that single
 * decision so it is unit-testable off the ESP; the actual present (epd_init/write/refresh
 * in epd_present()) stays ESP-woven in main.c. Mode-independent: the same gate protects
 * the on-device Berry path today and the server-frame path in a later wave.
 */
#ifndef PRESENT_CORE_H
#define PRESENT_CORE_H

#include <stdbool.h>

/* May the panel be presented this cycle?  render_ok is the return of the frame producer
 * (berry_render() today; the server fetch later). true  -> the framebuffer holds a valid,
 * fully-populated frame, present it through the content gate. false -> the producer failed
 * and s_fb must NOT be pushed to the panel; keep the last (bistable) frame. */
static inline bool present_gate_allows(bool render_ok)
{
    return render_ok;
}

#endif /* PRESENT_CORE_H */
