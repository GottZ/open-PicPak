/* Host render harness: run a Berry render script against the REAL main/fb.c on a PC and
 * dump the 30000-byte framebuffer -> the PNG (via host/fb2png.py) shows exactly what the
 * panel renders, no device or camera needed. The dev_* values here are stub placeholders.
 *
 * Build + run (from firmware/, needs Pillow for the PNG step and the Berry component fetched
 * by scripts/setup-berry.sh + the generated font16.h via host/gen_font16.py):
 *
 *   python3 host/gen_font16.py
 *   gcc -O1 -w -I main -I components/berry/src -I components/berry/generate -I components/berry \
 *       host/host_render.c main/fb.c \
 *       components/berry/src/*.c components/berry/port/be_port.c components/berry/port/be_modtab.c \
 *       -o build/host_render -lm
 *   ./build/host_render test/render16.be build/fb.bin
 *   python3 host/fb2png.py build/fb.bin build/fb.png      # -> build/fb.png (+ .x2)
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "berry.h"
#include "fb.h"

/* Stub device identity (format-preserving placeholders; on-device these come from
 * esp_read_mac() / the factory NVS). Locally-administered MAC, dummy serial. */
static int h_mac(bvm *vm)     { be_pushstring(vm, "02:00:00:00:00:01"); be_return(vm); }
static int h_bt_mac(bvm *vm)  { be_pushstring(vm, "02:00:00:00:00:02"); be_return(vm); }
static int h_chip(bvm *vm)    { be_pushstring(vm, "ESP32-C3");          be_return(vm); }
static int h_uptime(bvm *vm)  { be_pushint(vm, 1234);                   be_return(vm); }
static int h_reset(bvm *vm)   { be_pushstring(vm, "usb");               be_return(vm); }
static int h_batt_mv(bvm *vm) { be_pushint(vm, 3900);                   be_return(vm); }
static int h_batt_pct(bvm *vm){ be_pushint(vm, 70);                     be_return(vm); }
static int h_nvs(bvm *vm)     { (void)vm; be_return_nil(vm); }          /* no NVS on host */

int main(int argc, char **argv)
{
    if (argc < 3) { fprintf(stderr, "usage: %s script.be out.bin\n", argv[0]); return 2; }
    FILE *f = fopen(argv[1], "rb");
    if (!f) { perror("script"); return 2; }
    fseek(f, 0, SEEK_END); long n = ftell(f); fseek(f, 0, SEEK_SET);
    char *src = malloc(n + 1); if (fread(src, 1, n, f) != (size_t)n) { return 2; } src[n] = 0; fclose(f);

    bvm *vm = be_vm_new();
    fb_register(vm);
    be_regfunc(vm, "dev_mac", h_mac);
    be_regfunc(vm, "dev_bt_mac", h_bt_mac);
    be_regfunc(vm, "nvs_str", h_nvs);
    be_regfunc(vm, "dev_chip", h_chip);
    be_regfunc(vm, "dev_uptime_ms", h_uptime);
    be_regfunc(vm, "dev_reset", h_reset);
    be_regfunc(vm, "dev_batt_mv", h_batt_mv);
    be_regfunc(vm, "dev_batt_pct", h_batt_pct);

    int r = be_loadstring(vm, src);
    if (r == BE_OK) r = be_pcall(vm, 0);
    if (r != BE_OK) { be_dumpexcept(vm); return 1; }

    FILE *o = fopen(argv[2], "wb");
    if (!o) { perror("out"); return 2; }
    fwrite(fb_buffer(), 1, 30000, o);
    fclose(o);
    be_vm_delete(vm); free(src);
    fprintf(stderr, "rendered %s -> %s\n", argv[1], argv[2]);
    return 0;
}
