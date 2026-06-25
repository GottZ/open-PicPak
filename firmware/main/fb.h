/* Berry-native graphics stdlib for the PicPak 400x300 BWRY panel.
 * Exposes drawing primitives as global Berry functions; backs a single
 * 30000-byte framebuffer the firmware then streams via epd_write_full(). */
#pragma once
#include <stdint.h>
#include "berry.h"

const uint8_t *fb_buffer(void);   /* the 30000-byte EPD framebuffer */
uint8_t *fb_writable(void);       /* writable view of the same single framebuffer */
void fb_register(bvm *vm);        /* register fill/pixel/rect/disc/circle/triangle/text/dump */
void fb_dump_serial(void);        /* hex-dump the framebuffer over the console (FBDUMP_BEGIN..END) */
