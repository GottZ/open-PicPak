/* Berry-native graphics stdlib backing one 400x300 BWRY framebuffer.
 * Palette codes: 0=BLACK 1=WHITE 2=YELLOW 3=RED. 2bpp, 4px/byte, MSB-first.
 * The panel gate-scan is vertically mirrored, so px() packs at row (H-1-y);
 * the driver then streams the buffer linearly (epd_write_full). */
#include "fb.h"
#include "epd.h"
#include <stdio.h>
#include <string.h>
#include "font8x8.h"

#define W EPD_W
#define H EPD_H

static uint8_t s_fb[EPD_FRAME_BYTES];

const uint8_t *fb_buffer(void) { return s_fb; }

/* set one pixel, with bounds clip + vertical flip + 2bpp MSB-first packing */
static inline void px(int x, int y, int code)
{
    if (x < 0 || x >= W || y < 0 || y >= H) return;
    int idx = (H - 1 - y) * W + x;      /* vertical flip at the source */
    int b = idx >> 2;
    int sh = (3 - (idx & 3)) << 1;      /* leftmost pixel in the high bits */
    s_fb[b] = (uint8_t)((s_fb[b] & ~(3 << sh)) | ((code & 3) << sh));
}

static void fb_fill(int code)
{
    memset(s_fb, (code & 3) * 0x55, EPD_FRAME_BYTES);  /* same code in all 4 px of a byte */
}

static void fb_rect(int x, int y, int w, int h, int code, int filled)
{
    if (w <= 0 || h <= 0) return;
    if (filled) {
        for (int j = 0; j < h; j++)
            for (int i = 0; i < w; i++) px(x + i, y + j, code);
    } else {
        for (int i = 0; i < w; i++) { px(x + i, y, code); px(x + i, y + h - 1, code); }
        for (int j = 0; j < h; j++) { px(x, y + j, code); px(x + w - 1, y + j, code); }
    }
}

static void fb_disc(int cx, int cy, int r, int code)
{
    if (r < 0) return;
    for (int dy = -r; dy <= r; dy++)
        for (int dx = -r; dx <= r; dx++)
            if (dx * dx + dy * dy <= r * r) px(cx + dx, cy + dy, code);
}

static void fb_circle(int cx, int cy, int r, int code)  /* midpoint outline */
{
    if (r < 0) return;
    int x = r, y = 0, err = 1 - r;
    while (x >= y) {
        px(cx + x, cy + y, code); px(cx + y, cy + x, code);
        px(cx - y, cy + x, code); px(cx - x, cy + y, code);
        px(cx - x, cy - y, code); px(cx - y, cy - x, code);
        px(cx + y, cy - x, code); px(cx + x, cy - y, code);
        y++;
        if (err < 0) err += 2 * y + 1;
        else { x--; err += 2 * (y - x) + 1; }
    }
}

static int edge(int ax, int ay, int bx, int by, int px_, int py_)
{
    return (bx - ax) * (py_ - ay) - (by - ay) * (px_ - ax);
}

static void fb_triangle(int x0, int y0, int x1, int y1, int x2, int y2, int code)
{
    int minx = x0 < x1 ? (x0 < x2 ? x0 : x2) : (x1 < x2 ? x1 : x2);
    int maxx = x0 > x1 ? (x0 > x2 ? x0 : x2) : (x1 > x2 ? x1 : x2);
    int miny = y0 < y1 ? (y0 < y2 ? y0 : y2) : (y1 < y2 ? y1 : y2);
    int maxy = y0 > y1 ? (y0 > y2 ? y0 : y2) : (y1 > y2 ? y1 : y2);
    if (minx < 0) minx = 0;
    if (miny < 0) miny = 0;
    if (maxx >= W) maxx = W - 1;
    if (maxy >= H) maxy = H - 1;
    for (int y = miny; y <= maxy; y++) {
        for (int x = minx; x <= maxx; x++) {
            int w0 = edge(x1, y1, x2, y2, x, y);
            int w1 = edge(x2, y2, x0, y0, x, y);
            int w2 = edge(x0, y0, x1, y1, x, y);
            if ((w0 >= 0 && w1 >= 0 && w2 >= 0) || (w0 <= 0 && w1 <= 0 && w2 <= 0))
                px(x, y, code);
        }
    }
}

static void fb_text(int x, int y, const char *s, int code, int scale)
{
    if (scale < 1) scale = 1;
    for (int i = 0; s[i]; i++) {
        unsigned char ch = (unsigned char)s[i];
        if (ch >= 128) ch = '?';
        for (int r = 0; r < 8; r++) {
            unsigned char bits = (unsigned char)font8x8_basic[ch][r];   /* font8x8: bit0 = leftmost */
            for (int c = 0; c < 8; c++)
                if (bits & (1u << c)) {
                    int bx = x + (i * 8 + c) * scale, by = y + r * scale;
                    for (int sy = 0; sy < scale; sy++)
                        for (int sx = 0; sx < scale; sx++) px(bx + sx, by + sy, code);
                }
        }
    }
}

void fb_dump_serial(void)
{
    printf("\nFBDUMP_BEGIN\n");
    for (int i = 0; i < EPD_FRAME_BYTES; i++) {
        printf("%02x", s_fb[i]);
        if ((i & 63) == 63) { printf("\n"); fflush(stdout); }
    }
    printf("\nFBDUMP_END\n");
    fflush(stdout);
}

/* ---- Berry bindings (global functions) ---- */
static int l_fill(bvm *vm)   { fb_fill(be_toint(vm, 1)); be_return_nil(vm); }
static int l_pixel(bvm *vm)  { px(be_toint(vm,1), be_toint(vm,2), be_toint(vm,3)); be_return_nil(vm); }
static int l_rect(bvm *vm)
{
    int filled = be_top(vm) >= 6 ? be_tobool(vm, 6) : 1;
    fb_rect(be_toint(vm,1), be_toint(vm,2), be_toint(vm,3), be_toint(vm,4), be_toint(vm,5), filled);
    be_return_nil(vm);
}
static int l_disc(bvm *vm)   { fb_disc(be_toint(vm,1), be_toint(vm,2), be_toint(vm,3), be_toint(vm,4)); be_return_nil(vm); }
static int l_circle(bvm *vm) { fb_circle(be_toint(vm,1), be_toint(vm,2), be_toint(vm,3), be_toint(vm,4)); be_return_nil(vm); }
static int l_triangle(bvm *vm)
{
    fb_triangle(be_toint(vm,1), be_toint(vm,2), be_toint(vm,3), be_toint(vm,4),
                be_toint(vm,5), be_toint(vm,6), be_toint(vm,7));
    be_return_nil(vm);
}
static int l_text(bvm *vm)
{
    int scale = be_top(vm) >= 5 ? be_toint(vm, 5) : 1;
    fb_text(be_toint(vm,1), be_toint(vm,2), be_tostring(vm,3), be_toint(vm,4), scale);
    be_return_nil(vm);
}

static int l_dump(bvm *vm)   { (void)vm; fb_dump_serial(); be_return_nil(vm); }

void fb_register(bvm *vm)
{
    be_regfunc(vm, "dump",     l_dump);
    be_regfunc(vm, "fill",     l_fill);
    be_regfunc(vm, "pixel",    l_pixel);
    be_regfunc(vm, "rect",     l_rect);
    be_regfunc(vm, "disc",     l_disc);
    be_regfunc(vm, "circle",   l_circle);
    be_regfunc(vm, "triangle", l_triangle);
    be_regfunc(vm, "text",     l_text);
}
