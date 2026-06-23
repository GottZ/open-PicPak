/* EPD driver for GDEY0420F51-class panels (4.2" 400x300 BWRY, controller HX8717).
 * Init sequence ported from GxEPD2 GxEPD2_420c_GDEY0420F51 (Good Display demo).
 * Pixel format: 4 px/byte, 2 bit/px, MSB-first; code 0=K 1=W 2=Y 3=R (= picpak format).
 */
#pragma once
#include <stdint.h>
#include <stddef.h>

#define EPD_W 400
#define EPD_H 300
#define EPD_FRAME_BYTES (EPD_W * EPD_H / 4)   /* 30000 */

void epd_init(void);                       /* SPI+GPIO, reset, panel init, power on */
void epd_write_full(const uint8_t *buf);   /* 30000 B into RAM (cmd 0x10) */
void epd_refresh(void);                    /* display refresh + wait */
void epd_sleep(void);                      /* power off + deep sleep */
